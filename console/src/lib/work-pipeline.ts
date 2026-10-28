import { arr, type Json, str } from "@/lib/compound";
import { ago } from "@/lib/time";
import { type StateTone, toneForState } from "@/components/kit/StateBadge";
import { WORK_TRANSITIONS, workOptionsFor } from "@/lib/decision-registry";
import { shortText, workId } from "@/lib/work-data";

/*
 * Pipeline lens data model — maps the live workbench / suggestion / source
 * projections onto the seven visible stages the operator scans left to right:
 *
 *   Signal → Suggest → Add to work → Start → Run → Proof → Learn
 *
 *
 * Every card that carries a real work_id is `traceable` — clicking it opens the
 * Trace drawer. Pure-ish server module (reads projections already fetched by
 * the page); returns only serializable shapes so a client lens can render them.
 */

/**
 * Envelope-stage position for the SegmentedProgress on a card (§9.3): where in
 * the seven-stage spine (Signal→Suggest→Add to work→Start→Run→Proof→Learn) the
 * item sits. `done` = stages complete, `active` = the current stage (null when
 * terminal/stalled — nothing pulses). Derived from the SAME state buckets
 * buildPipeline scans, so the strip and the progress can never disagree.
 */
export type StageProgress = { total: number; done: number; active: number | null };

const STAGE_TOTAL = 7;

function progressAt(active: number, opts: { complete?: boolean; stalled?: boolean } = {}): StageProgress {
  if (opts.complete) return { total: STAGE_TOTAL, done: STAGE_TOTAL, active: null };
  const a = Math.max(0, Math.min(STAGE_TOTAL - 1, active));
  return { total: STAGE_TOTAL, done: a, active: opts.stalled ? null : a };
}

/** Map a lifecycle state onto its envelope-stage progress (§9.3). */
export function stageProgressForState(state: string): StageProgress {
  switch (state) {
    case "proposed":
    case "suggested":
    case "candidate":
      return progressAt(2); // completed Signal+Suggest, waiting to be added
    case "admitted":
    case "ready":
      return progressAt(3); // Start
    case "claimed":
      return progressAt(4, { stalled: true }); // in the Execute lane but not yet running
    case "in_progress":
    case "running":
    case "verifying":
      return progressAt(4); // Execute — the breathing stage
    case "blocked":
    case "held":
    case "needs_review":
      return progressAt(4, { stalled: true }); // stalled on the operator at Execute
    case "completed":
    case "promoted":
    case "filed":
    case "allow":
      return progressAt(6, { complete: true }); // through Proof + Learn
    case "failed":
    case "deny":
    case "rejected":
      return progressAt(4, { stalled: true });
    case "cancelled":
    case "demoted":
    case "no_promote":
      return progressAt(2, { stalled: true });
    default:
      return progressAt(2);
  }
}

export type BoardCard = {
  key: string;
  workId: string; // "" for a pure suggestion with no work yet
  displayId: string;
  title: string;
  subtitle: string;
  state: string;
  tone: StateTone;
  ageLabel: string;
  moveType: string;
  note: string;
  traceable: boolean;
  running: boolean;
  /** envelope-stage position for the card's SegmentedProgress (§9.3). */
  progress: StageProgress;
};

export type RailNode = "empty" | "filled" | "seat" | "done" | "run";

export type SignalTile = { count: number; href: string; label: string; sub: string };

export type PipelineStageModel = {
  key: string;
  label: string;
  index: number;
  tooltip: string;
  emptyCopy: string;
  count: number;
  node: RailNode;
  seat: boolean;
  cards: BoardCard[];
  signal?: SignalTile;
};

export type BoardColumnModel = {
  key: string;
  label: string;
  description: string;
  tone: StateTone;
  emptyCopy: string;
  cards: BoardCard[];
};

export type OfferedPacketModel = {
  key: string;
  title: string;
  surface: string;
  path: string;
  state: string;
  tone: StateTone;
};

/** One contextual governed transition offered on the work-advance state machine. */
export type ActionOption = {
  value: string;
  label: string;
  tone: string;
  description: string;
  argv: string[] | null;
  terminal: boolean;
  needsSummary: boolean;
  needsMoveType: boolean;
};

export type WorkActionRowData = {
  area: string;
  itemId: string;
  state: string;
  hasTransitions: boolean;
  options: ActionOption[];
};

const TERMINAL_TARGETS = new Set(["completed", "failed", "cancelled"]);

/**
 * Build the contextual action row for a work id + state directly from the
 * EXISTING work-advance state machine (WORK_TRANSITIONS / workOptionsFor in
 * decision-registry — consumed, never edited). Shared by the Trace API and the
 * /work/[id] page so the drawer and the detail page can never diverge on what
 * moves are legal.
 */
export function buildActionRow(id: string, state: string): WorkActionRowData {
  const options: ActionOption[] = workOptionsFor({ work_id: id, state }).map((option) => ({
    value: option.value,
    label: option.label,
    tone: option.tone,
    description: option.description,
    argv: option.argv ?? null,
    terminal: TERMINAL_TARGETS.has(option.value),
    needsSummary: TERMINAL_TARGETS.has(option.value),
    needsMoveType: option.value === "completed" || option.value === "failed",
  }));
  return {
    area: "work-advance",
    itemId: id,
    state,
    hasTransitions: (WORK_TRANSITIONS[state] ?? []).length > 0,
    options,
  };
}

const LEARN_MOVES = new Set(["memory_delta", "rule_delta", "control_plane_repair", "outward_delta"]);

function cardTitle(item: Json): string {
  const surface = str(item.canonical_surface);
  if (surface.startsWith("packet:")) return surface.slice("packet:".length).replace(/-/g, " ");
  return str(item.success_predicate) || str(item.desired_outcome) || str(item.title) || workId(item) || "work item";
}

function workCard(item: Json, note = "", progress?: StageProgress): BoardCard {
  const id = workId(item);
  const state = str(item.state);
  const age = str(item.claimed_at) || str(item.created_at) || str(item.updated_at);
  return {
    key: id || cardTitle(item),
    workId: id,
    displayId: id,
    title: cardTitle(item),
    subtitle: str(item.claimed_by) || str(item.owner) || "",
    state,
    tone: toneForState(state),
    ageLabel: age ? ago(age) : "",
    moveType: str(item.move_type),
    note,
    traceable: Boolean(id),
    running: state === "running",
    progress: progress ?? stageProgressForState(state),
  };
}

function proposalCard(p: Json): BoardCard {
  const id = str(p.id);
  return {
    key: id || str(p.title),
    workId: "",
    displayId: id,
    title: str(p.title) || str(p.predicate) || str(p.kind) || "suggested work",
    subtitle: str(p.capability_class) || "suggestion",
    state: "proposed",
    tone: "draft",
    ageLabel: "",
    moveType: "",
    note: shortText(str(p.consequence) || str(p.summary), 120),
    traceable: false,
    running: false,
    progress: progressAt(1), // a suggestion sits at Suggest
  };
}

function admissionCard(ap: Json): BoardCard {
  const id = str(ap.work_id);
  const state = str(ap.work_state) || "proposed";
  const stale = str(ap.staleness) === "stale";
  return {
    key: id || str(ap.plan_id),
    workId: id,
    displayId: id || str(ap.plan_id),
    title: `Intent: ${str(ap.success_predicate) || str(ap.desired_outcome) || "work plan"}`,
    subtitle: str(ap.work_owner) || str(ap.actor) || "suggestion",
    state,
    tone: toneForState(state),
    ageLabel: str(ap.age) || (ap.generated_at ? ago(ap.generated_at) : ""),
    moveType: "",
    note: stale ? "stale plan - revalidate before adding" : shortText(str(ap.next_action), 96),
    traceable: Boolean(id),
    running: false,
    progress: stageProgressForState(state),
  };
}

const STAGE_COPY: Record<string, { label: string; tooltip: string; empty: string }> = {
  signal: {
    label: "Signal",
    tooltip: "Where new information enters — connected sources and their freshness.",
    empty: "No sources wired yet.",
  },
  propose: {
    label: "Suggest",
    tooltip: "Bounded jobs suggested from live evidence. Nothing advances without you.",
    empty: "No open suggestions.",
  },
  admit: {
    label: "Add to work",
    tooltip: "Add a job to the work queue; it does not run anything yet.",
    empty: "Nothing waits on you to add.",
  },
  claim: {
    label: "Start",
    tooltip: "An executor starts under a lease, so two agents never collide.",
    empty: "Nothing waiting for an executor.",
  },
  execute: {
    label: "Run",
    tooltip: "Rented executors do the work inside policy. Every mutation needs a PolicyDecision.",
    empty: "Nothing running right now.",
  },
  receipt: {
    label: "Proof",
    tooltip: "Proof a job ran — what was touched, what it cost, what came out.",
    empty: "No fresh proof.",
  },
  learn: {
    label: "Learn",
    tooltip: "The write-back: outcomes become memory, rules, and skill-fitness that change the next run.",
    empty: "No recent write-backs. audit_only counts for nothing.",
  },
};

function stage(
  key: string,
  index: number,
  cards: BoardCard[],
  opts: { seat?: boolean; node?: RailNode; signal?: SignalTile } = {},
): PipelineStageModel {
  const copy = STAGE_COPY[key];
  const has = cards.length > 0 || Boolean(opts.signal && opts.signal.count > 0);
  const node: RailNode =
    opts.node ?? (!has ? "empty" : opts.seat ? "seat" : "filled");
  return {
    key,
    label: copy.label,
    index,
    tooltip: copy.tooltip,
    emptyCopy: copy.empty,
    count: opts.signal ? opts.signal.count : cards.length,
    node,
    seat: Boolean(opts.seat),
    cards,
    signal: opts.signal,
  };
}

export function buildPipeline(
  workbench: Json | null,
  sources: Json | null,
  proposals: Json[],
): PipelineStageModel[] {
  const open = arr(workbench?.open);
  const recent = arr(workbench?.recent);
  const admissionPreviews = arr(workbench?.admission_previews);
  const byStates = (states: string[]) => open.filter((w) => states.includes(str(w.state)));

  const proposedOpen = byStates(["proposed"]);
  const proposedOpenIds = new Set(proposedOpen.map(workId).filter(Boolean));

  const connections = arr(sources?.connections ?? sources?.sources ?? sources?.rows);
  const freshCount = connections.filter((c) => str(c.staleness) === "fresh" || str(c.state) === "fresh").length;

  const proposeCards: BoardCard[] = [
    ...proposals.slice(0, 6).map(proposalCard),
    ...admissionPreviews
      .filter((ap) => !proposedOpenIds.has(str(ap.work_id)))
      .slice(0, 6)
      .map(admissionCard),
  ];

  const admitCards = proposedOpen.map((w) => workCard(w, "suggested -> add to work"));
  const claimCards = byStates(["admitted", "ready"]).map((w) => workCard(w));
  const executeCards = byStates(["claimed", "in_progress", "running", "verifying"]).map((w) => workCard(w));
  const receiptCards = recent
    .filter((w) => str(w.receipt_id))
    .slice(0, 8)
    .map((w) => workCard(w));
  const learnCards = recent
    .filter((w) => LEARN_MOVES.has(str(w.move_type)))
    .slice(0, 8)
    .map((w) => workCard(w, str(w.move_type).replace(/_/g, " ")));

  return [
    stage("signal", 1, [], {
      signal: {
        count: connections.length,
        href: "/sources",
        label: connections.length === 1 ? "source" : "sources",
        sub: connections.length ? `${freshCount} fresh` : "none wired",
      },
    }),
    stage("propose", 2, proposeCards),
    stage("admit", 3, admitCards, { seat: true }),
    stage("claim", 4, claimCards),
    stage("execute", 5, executeCards, {
      node: executeCards.some((c) => c.running) ? "run" : executeCards.length ? "filled" : "empty",
    }),
    stage("receipt", 6, receiptCards, { node: receiptCards.length ? "done" : "empty" }),
    stage("learn", 7, learnCards, { node: learnCards.length ? "done" : "empty" }),
  ];
}

export function buildBoard(open: Json[], recent: Json[]): BoardColumnModel[] {
  const byStates = (states: string[]) => open.filter((w) => states.includes(str(w.state)));
  const columns: { key: string; label: string; description: string; tone: StateTone; states: string[]; empty: string }[] = [
    { key: "inbox", label: "Inbox", description: "Suggested tasks waiting to be added.", tone: "draft", states: ["proposed", "admitted"], empty: "Nothing suggested." },
    { key: "ready", label: "Ready", description: "Waiting for an executor to start.", tone: "queued", states: ["ready"], empty: "Nothing ready to start." },
    { key: "running", label: "Running", description: "Started, in progress, or being checked.", tone: "live", states: ["claimed", "in_progress", "running", "verifying"], empty: "Nothing running." },
    { key: "needs-me", label: "Needs me", description: "Blocked or held on your decision.", tone: "seam", states: ["blocked", "held"], empty: "Clean seam. Nothing waits on you." },
  ];
  return columns.map((col) => ({
    key: col.key,
    label: col.label,
    description: col.description,
    tone: col.tone,
    emptyCopy: col.empty,
    cards: byStates(col.states).map((w) => workCard(w)),
  }));
}

export function buildOffered(packets: Json[]): OfferedPacketModel[] {
  return packets
    .filter((p) => (str(p.state) || str(p.status)).startsWith("offered"))
    .map((p) => {
      const state = str(p.state) || str(p.status);
      return {
        key: str(p.canonical_surface) || str(p.path) || str(p.title),
        title: str(p.title) || str(p.canonical_surface) || "packet",
        surface: str(p.canonical_surface),
        path: str(p.path) || str(p.source_ref),
        state,
        tone: state.startsWith("offered") ? "draft" : toneForState(state.split(/[\s(]/)[0]),
      };
    });
}
