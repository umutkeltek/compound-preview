"use client";

import * as Dialog from "@radix-ui/react-dialog";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";

import { CopyCmd } from "@/components/CopyCmd";
import { AuthorityFaceDetails } from "@/components/authority/AuthorityFaceDetails";
import { Button, FreshnessChip, StateBadge, Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/kit";
import {
  decideReadinessSections,
  enumerableAuthorityFaces,
  queueSystemicBlocker,
  remainingDecideMembers,
  type EnumerableAuthorityFace,
  type SystemicBlocker,
} from "@/lib/decide-members";
import { genericReviewResolutionActs } from "@/lib/decide-resolution-acts";
import type { DecideBrief, DecideBriefOption, DecideDomain, DecideGroup, DecideMember } from "@/lib/decide-model";
import type { DecisionEffectKind } from "@/lib/decision-registry";
import { buildEnumerableActionRequest, decideActIdentity, RATIONALE_REQUIRED_MESSAGE } from "@/lib/governed-action-client";
import { OPERATOR_INPUTS } from "@/lib/operator-inputs";
import { blockedReasonPlain, ownerUnavailableMessage } from "@/lib/glossary";
import { GovernedAct, type ExecuteResult } from "./GovernedAct";
import { PanelProofContinuity } from "./PanelProofContinuity";

/* Domain hue is accent-only (glyph tile + dot); the needs-you seam is amber. */
const DOMAIN_TILE: Record<DecideDomain, string> = {
  work: "border-work-line bg-work-soft text-work",
  memory: "border-memory-line bg-memory-soft text-memory",
  capability: "border-capability-line bg-capability-soft text-capability",
  route: "border-route-line bg-route-soft text-route",
  product: "border-product-line bg-product-soft text-product",
  neutral: "border-hair bg-soft-2 text-muted",
};

function operatorText(value: string): string {
  return value
    .replace(/\bclaude_code\b/gi, "Claude Code")
    .replace(/\bswitchback_api\b/gi, "Switchback API");
}

function actorLabel(value: string): string {
  return value
    .split(/[_-]+/)
    .filter(Boolean)
    .map((part) => {
      const lower = part.toLowerCase();
      if (lower === "api") return "API";
      if (lower === "codex") return "Codex";
      if (lower === "claude") return "Claude";
      if (lower === "switchback") return "Switchback";
      return part.charAt(0).toUpperCase() + part.slice(1);
    })
    .join(" ");
}

function GlyphTile({ domain, glyph }: { domain: DecideDomain; glyph: string }) {
  return (
    <span
      aria-hidden
      className={`inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-sm border text-xs font-bold ${DOMAIN_TILE[domain]}`}
    >
      {glyph}
    </span>
  );
}

function ConsequencePair({ approve, hold }: { approve: string; hold: string }) {
  return (
    <div className="mt-2 rounded-md bg-soft px-3 py-2 text-xs leading-relaxed">
      <div className="text-ink">
        <span className="font-semibold text-good">if you act</span>
        <span className="text-faint"> → </span>
        {approve}
      </div>
      <div className="mt-0.5 text-muted">
        <span className="font-semibold">if you wait</span>
        <span> → </span>
        {hold}
      </div>
    </div>
  );
}

/* ---------- decision-brief translation (propose-only; display-only) ----------
 * Renders ONLY when a card's group carries a prepared, already-validated
 * `brief` (see decide-model.ts groupBrief — scans every group member, not
 * just group.sample). Un-briefed cards render with `group.brief` undefined/
 * null, so this whole block is absent and every byte above/below it is
 * exactly what rendered before this packet (the frozen-markup guarantee).
 * `command_argv` is copy/display material ONLY — never wired to a fetch, so
 * a brief can never introduce a new execution path (see AGENTS.md boundary:
 * "Do not wire brief-sourced argv into /api/decision/execute").
 */
function BriefOptionRow({ option, recommended }: { option: DecideBriefOption; recommended: boolean }) {
  const argv = option.commandArgv;
  const copyCmd = argv && argv.length ? (argv[0] === "compound" ? argv.join(" ") : `compound ${argv.join(" ")}`) : "";
  return (
    <div
      className={`rounded-md border px-3 py-2 text-xs leading-relaxed ${
        recommended ? "border-good/40 bg-good/5" : "border-hair bg-soft"
      }`}
    >
      <div className="flex items-center gap-1.5">
        <span className="font-semibold text-ink">{option.label || option.value}</span>
        {recommended ? (
          <span className="rounded-pill bg-good/15 px-1.5 py-0.5 text-2xs font-semibold text-good">recommended</span>
        ) : null}
      </div>
      {option.consequenceApprove ? (
        <div className="mt-1 text-ink">
          <span className="font-semibold text-good">if chosen</span>
          <span className="text-faint"> → </span>
          {option.consequenceApprove}
        </div>
      ) : null}
      {option.consequenceHold ? (
        <div className="mt-0.5 text-muted">
          <span className="font-semibold">if not</span>
          <span> → </span>
          {option.consequenceHold}
        </div>
      ) : null}
      {copyCmd ? (
        <div className="mt-1.5">
          <CopyCmd cmd={copyCmd} compact />
        </div>
      ) : null}
    </div>
  );
}

function BriefPanel({ brief }: { brief: DecideBrief }) {
  return (
    <div className="mt-3 rounded-md border border-hair bg-page px-3 py-2.5">
      <div className="flex items-start justify-between gap-2">
        <div className="text-sm font-semibold leading-snug text-ink">{brief.headline}</div>
        <span
          className="shrink-0 rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs font-medium text-muted"
          title="This translation was prepared by an agent session for you — not the operator, not auto-applied."
        >
          prepared by {actorLabel(brief.preparedBy)}
        </span>
      </div>
      {brief.whatThisIs ? <p className="mt-1 text-xs leading-relaxed text-muted">{brief.whatThisIs}</p> : null}
      {brief.findings.length ? (
        <ul className="mt-2 flex flex-col gap-1">
          {brief.findings.map((f, i) => (
            <li key={i} className="text-xs leading-relaxed text-ink">
              <span>{f.finding}</span>{" "}
              {f.evidenceRef ? (
                <code className="mono text-2xs text-faint" title={f.evidenceRef}>
                  {f.evidenceRef}
                </code>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}
      {brief.options.length ? (
        <div className="mt-2 flex flex-col gap-1.5">
          {brief.options.map((o, i) => (
            <BriefOptionRow
              key={o.value || i}
              option={o}
              recommended={Boolean(brief.recommendation.option) && o.value === brief.recommendation.option}
            />
          ))}
        </div>
      ) : null}
      {brief.recommendation.rationale ? (
        <p className="mt-2 text-2xs leading-relaxed text-faint">
          <span className="font-semibold text-muted">why this lean</span> — {brief.recommendation.rationale}
          {brief.recommendation.confidence ? ` (confidence ${Math.round(brief.recommendation.confidence * 100)}%)` : ""}
        </p>
      ) : null}
    </div>
  );
}

/* ---------- enumerable batch: per-member checkboxes + exclusion ---------- */

const ENUMERABLE_ACTION_COPY: Record<DecisionEffectKind, {
  imperative: string;
  progressive: string;
  past: string;
  completed: string;
  confirmationLead: string;
}> = {
  apply: {
    imperative: "Apply",
    progressive: "Applying",
    past: "Applied",
    completed: "applied",
    confirmationLead: "Applies the selected action to",
  },
  record: {
    imperative: "Record",
    progressive: "Recording",
    past: "Recorded",
    completed: "recorded",
    confirmationLead: "Records the reviewed decision for",
  },
  track: {
    imperative: "Plan",
    progressive: "Planning",
    past: "Planned",
    completed: "planned",
    confirmationLead: "Plans the tracked step for",
  },
};

function EnumerableApply({
  area,
  members,
  excluded,
  onToggle,
  mutationsEnabled,
  requiresConfirmation,
  requiresRationale,
  effectKind,
  policyNote,
  label,
  authorityBound,
  representedCount,
  unavailableCount,
}: {
  area: string;
  members: DecideMember[];
  excluded: Set<string>;
  onToggle: (id: string) => void;
  mutationsEnabled: boolean;
  requiresConfirmation: boolean;
  requiresRationale: boolean;
  effectKind: DecisionEffectKind;
  policyNote: string;
  label: string;
  authorityBound: boolean;
  representedCount: number;
  unavailableCount: number;
}) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [running, setRunning] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [rationale, setRationale] = useState("");
  const [completed, setCompleted] = useState<Set<string>>(() => new Set());

  const included = remainingDecideMembers(members, excluded, completed);
  // Operator-input areas label the confirm textarea from the shared spec
  // (Rationale for review-record areas; Expected effect for hypothesis
  // approval) instead of hardcoding the rationale wording.
  const operatorInput = OPERATOR_INPUTS[area];
  const actionCopy = ENUMERABLE_ACTION_COPY[effectKind];
  const isSingleton = representedCount === 1 && members.length === 1;
  const singletonMember = isSingleton ? members[0] : null;
  const scopeLabel = `${members.length} ready of ${representedCount}`;
  const unchangedLabel = unavailableCount > 0
    ? `${unavailableCount} unavailable ${unavailableCount === 1 ? "member remains" : "members remain"} unchanged.`
    : "";
  const selectedAuthorityFaces = authorityBound ? enumerableAuthorityFaces(included) : [];
  const authorityFacesComplete = selectedAuthorityFaces !== null;

  const apply = useCallback(async (operatorRationale: string) => {
    if (!mutationsEnabled || running || included.length === 0 || !authorityFacesComplete) return;
    if (requiresRationale && !operatorRationale.trim()) {
      setError(RATIONALE_REQUIRED_MESSAGE);
      return;
    }
    setRunning(true);
    setError(null);
    setProgress({ done: 0, total: included.length });
    let failed = "";
    let completedThisRun = 0;
    for (let i = 0; i < included.length; i++) {
      const member = included[i];
      try {
        const response = await fetch("/api/decision/execute", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify(buildEnumerableActionRequest(
            area,
            member,
            requiresConfirmation,
            operatorRationale.trim(),
          )),
        });
        const body = (await response.json().catch(() => null)) as { ok?: boolean; stderr?: string } | null;
        if (!response.ok || !body?.ok) {
          failed = body?.stderr || `member ${member.id} failed (${response.status})`;
          break;
        }
      } catch (e) {
        failed = e instanceof Error ? e.message : "request failed";
        break;
      }
      completedThisRun = i + 1;
      setCompleted((previous) => new Set(previous).add(member.id));
      setProgress({ done: completedThisRun, total: included.length });
    }
    setRunning(false);
    if (failed) {
      setProgress({ done: completedThisRun, total: included.length });
      setError(completedThisRun > 0 ? `${actionCopy.past} ${completedThisRun} of ${included.length}; ${failed}. Refreshing owner truth.` : failed);
      router.refresh();
    }
    else {
      setOpen(false);
      router.refresh();
    }
  }, [actionCopy.past, area, authorityFacesComplete, included, mutationsEnabled, requiresConfirmation, requiresRationale, running, router]);

  const trigger = useCallback(() => {
    if (!mutationsEnabled || running || included.length === 0 || !authorityFacesComplete) return;
    setError(null);
    if (requiresConfirmation) {
      setOpen(true);
      return;
    }
    void apply("");
  }, [apply, authorityFacesComplete, included.length, mutationsEnabled, requiresConfirmation, running]);

  return (
    <div className="flex flex-col gap-2">
      {unavailableCount > 0 ? (
        <section className="rounded-md border border-seam-line bg-seam-soft px-3 py-2" aria-label="Executable member scope">
          <div className="text-xs font-semibold text-seam">{scopeLabel}</div>
          <p className="mt-0.5 text-2xs leading-relaxed text-muted">{unchangedLabel}</p>
        </section>
      ) : null}
      <ul className="flex flex-col gap-1">
        {members.map((m) => {
          const isCompleted = completed.has(m.id);
          const isIncluded = !excluded.has(m.id) && !isCompleted;
          return (
            <li key={m.id}>
              <label
                title={m.id}
                className={`flex min-h-8 cursor-pointer items-center gap-2 rounded-sm px-2 py-1 ${isCompleted ? "cursor-default opacity-60" : "hover:bg-soft"}`}
              >
                <input
                  type="checkbox"
                  checked={isIncluded}
                  disabled={isCompleted}
                  onChange={() => onToggle(m.id)}
                  className="h-4 w-4 shrink-0 accent-work"
                />
                <span className="min-w-0 flex-1" title={m.label}>
                  <span className="line-clamp-2 block text-xs leading-snug text-muted">
                    {m.label}{isCompleted ? ` · ${actionCopy.completed}` : ""}
                  </span>
                  {m.proposedAtLabel ? (
                    <time dateTime={m.proposedAt} className="mt-0.5 block text-2xs tabular-nums text-faint">
                      Proposed {m.proposedAtLabel}
                    </time>
                  ) : null}
                </span>
              </label>
            </li>
          );
        })}
      </ul>
      {authorityBound && selectedAuthorityFaces ? (
        <EnumerableAuthorityList members={selectedAuthorityFaces} />
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="primary"
          disabled={!mutationsEnabled || running || included.length === 0 || !authorityFacesComplete}
          onClick={trigger}
          title={mutationsEnabled ? undefined : "Launch via 'compound console --operator' to act from a pinned console."}
        >
          {running
            ? isSingleton
              ? `${actionCopy.progressive}…`
              : `${actionCopy.progressive} ${progress?.done ?? 0}/${progress?.total ?? included.length}…`
            : isSingleton
              ? label
              : unavailableCount > 0
                ? requiresConfirmation
                  ? `Review ${included.length} ready of ${representedCount}`
                  : `${actionCopy.imperative} ${included.length} ready of ${representedCount}`
              : requiresConfirmation
              ? `Review ${included.length} of ${members.length}`
              : `${actionCopy.imperative} ${included.length} of ${members.length}`}
        </Button>
        <span className="text-2xs text-faint">
          {unavailableCount > 0
            ? `Only ready members can run. ${unchangedLabel}`
            : isSingleton && policyNote
            ? policyNote
            : `Each member is ${actionCopy.completed} on its own; excluded members are skipped.`}
        </span>
      </div>
      {!open && error ? <span role="alert" className="text-2xs leading-snug text-crit">{error}</span> : null}
      {authorityBound && !authorityFacesComplete ? (
        <span role="alert" className="text-2xs leading-snug text-crit">
          Current member authority is incomplete. Refresh before acting.
        </span>
      ) : null}

      <Dialog.Root open={open} onOpenChange={(next) => { if (!running) setOpen(next); }}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 z-50 bg-strong/35 backdrop-blur-sm motion-reduce:backdrop-blur-none" />
          <Dialog.Content className="fixed top-1/2 left-1/2 z-50 max-h-[calc(100vh-2rem)] w-[calc(100vw-2rem)] max-w-md -translate-x-1/2 -translate-y-1/2 overflow-y-auto overscroll-contain rounded-lg border border-hair bg-panel p-5 shadow-lg focus:outline-none">
            <Dialog.Title className="text-md font-semibold text-ink">Confirm {label}</Dialog.Title>
            <Dialog.Description className="mt-2 text-sm leading-relaxed text-muted">
              {isSingleton ? (
                <>This will run <span className="font-medium text-ink">“{label}”</span> for {singletonMember?.label ?? "this item"}.</>
              ) : (
                <>{actionCopy.confirmationLead} {included.length} selected ready members of {representedCount}, one at a time. {unchangedLabel || "Excluded members remain unchanged."} Completed members are not retried.</>
              )}
              {policyNote ? (
                <span className="mt-3 block rounded-md border border-hair bg-soft px-3 py-2 text-xs leading-relaxed text-muted">
                  <span className="font-semibold text-ink">Owner note:</span> {policyNote}
                </span>
              ) : null}
            </Dialog.Description>
            {authorityBound && selectedAuthorityFaces ? (
              <EnumerableAuthorityList members={selectedAuthorityFaces} className="mt-4" />
            ) : null}
            {requiresRationale ? (
              <label className="mt-3 flex flex-col gap-1">
                <span className="text-2xs font-bold uppercase text-faint">{operatorInput?.label ?? "Rationale"} <span className="text-crit">*</span></span>
                <textarea
                  value={rationale}
                  onChange={(event) => { setRationale(event.target.value); setError(null); }}
                  required
                  placeholder={operatorInput?.placeholder ?? "Why is this the right reviewed action? This becomes decision provenance."}
                  className="min-h-24 resize-y rounded-md border border-hair bg-panel px-3 py-2 text-sm text-ink outline-none focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
                />
              </label>
            ) : null}
            {error ? (
              <div role="alert" aria-live="assertive" className="mt-3 rounded-md border border-crit-line bg-crit-soft px-3 py-2 text-xs leading-relaxed text-crit">
                {error}
              </div>
            ) : null}
            <div className="mt-4 flex justify-end gap-2">
              <Dialog.Close asChild><Button variant="secondary" disabled={running}>Cancel</Button></Dialog.Close>
              <Button
                variant="primary"
                disabled={running}
                onClick={() => void apply(rationale)}
                title={requiresRationale && !rationale.trim() ? RATIONALE_REQUIRED_MESSAGE : undefined}
              >
                {running
                  ? isSingleton
                    ? `${actionCopy.progressive}…`
                    : `${actionCopy.progressive} ${progress?.done ?? 0}/${progress?.total ?? included.length}…`
                  : isSingleton
                    ? `Confirm ${label}`
                    : `Confirm ${included.length}`}
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </div>
  );
}

/* ---------- read-only member list (atomic-run / bespoke / copy) ---------- */

function ReadonlyMembers({ members, atomic }: { members: DecideMember[]; atomic: boolean }) {
  if (members.length <= 1) return null;
  return (
    <details className="mt-3 rounded-md border border-hair bg-soft px-3 py-2 text-xs text-muted">
      <summary className="cursor-pointer font-medium text-muted">Members · {members.length}</summary>
      {atomic ? (
        <p className="mt-2 text-2xs leading-relaxed text-faint">
          This act is run-scoped — it applies to every member together; members cannot be excluded one by one.
        </p>
      ) : null}
      <ul className="mt-2 flex flex-col gap-1">
        {members.slice(0, 60).map((m) => (
          <li key={m.id}>
            <span className="block min-w-0 truncate text-2xs text-muted" title={m.id}>
              {m.label}
            </span>
          </li>
        ))}
      </ul>
      {members.length > 60 ? (
        <p className="mt-1 text-2xs text-faint">+{members.length - 60} more not shown.</p>
      ) : null}
    </details>
  );
}

const CAPABILITY_TONE: Record<DecideGroup["capability"]["kind"], string> = {
  apply: "border-good-line bg-good-soft text-good",
  record: "border-hair bg-soft text-muted",
  track: "border-work-line bg-work-soft text-work",
  unavailable: "border-seam-line bg-seam-soft text-seam",
};

function CapabilityStatus({ group, compact = false }: { group: DecideGroup; compact?: boolean }) {
  // The compact badge stays terse by design (a status pill, not a sentence);
  // the hover title carries the named-owner detail so even the list row
  // never leaves an operator with just the bare "Owner action unavailable"
  // label and nothing else.
  const detail = group.capability.kind === "unavailable" && group.reviewResolutionItemId
    ? ownerUnavailableMessage(group.area)
    : group.capability.detail;
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-pill border font-semibold ${CAPABILITY_TONE[group.capability.kind]} ${
        compact ? "px-2 py-0.5 text-2xs" : "px-2.5 py-1 text-xs"
      }`}
      title={detail}
    >
      {group.capability.label}
    </span>
  );
}

function EnumerableAuthorityList({
  members,
  className = "",
}: {
  members: EnumerableAuthorityFace[];
  className?: string;
}) {
  return (
    <section className={`${className} flex flex-col gap-3`} aria-label="Selected member authorities">
      {members.map((member) => (
        <div key={member.id} className="rounded-md border border-hair bg-soft p-2">
          <div className="mb-2 text-xs font-semibold leading-snug text-ink">{member.label}</div>
          <AuthorityFaceDetails authority={member.authority} embedded />
        </div>
      ))}
    </section>
  );
}

function DecisionRow({
  group,
  selected,
  rowRef,
  onActivate,
  onFocus,
  onKeyDown,
  demoteBlockedStamp,
}: {
  group: DecideGroup;
  selected: boolean;
  rowRef: (element: HTMLButtonElement | null) => void;
  onActivate: () => void;
  onFocus: () => void;
  onKeyDown: (event: React.KeyboardEvent<HTMLButtonElement>) => void;
  /** True when a queue-level systemic-blocker banner already explains this
   * exact reason once for the whole queue — the per-row stamp goes quiet
   * instead of repeating it. */
  demoteBlockedStamp: boolean;
}) {
  return (
    <button
      ref={rowRef}
      type="button"
      aria-pressed={selected}
      onClick={onActivate}
      onFocus={onFocus}
      onKeyDown={onKeyDown}
      className={`grid w-full grid-cols-[auto_1fr] gap-3 border-b border-l-2 border-b-hair px-3 py-3 text-left outline-none transition-colors last:border-b-0 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus ${
        selected ? "border-l-seam bg-soft" : "border-l-transparent bg-panel hover:bg-soft"
      }`}
    >
      <GlyphTile domain={group.domain} glyph={group.glyph} />
      <span className="min-w-0">
        <span className="flex items-center justify-between gap-2">
          <span className="truncate text-2xs font-bold uppercase text-faint" title={group.areaPlain}>{group.areaLabel}</span>
          <time
            dateTime={group.face.proposedAt || undefined}
            title={group.face.proposedAtLabel || "Proposal time unavailable"}
            className={`shrink-0 text-2xs tabular-nums ${group.old ? "font-semibold text-seam" : "text-faint"}`}
          >
            {group.ageLabel}
          </time>
        </span>
        {/* Row headline is the real object text, not a category template —
         * the same WHAT the detail card leads with (decide-model.ts's
         * objectText). Falls back to the title for the (common) areas where
         * title already carries the real headline. */}
        <span className="mt-1 line-clamp-2 block text-sm font-semibold leading-snug text-ink">
          {operatorText(group.objectText || group.title)}
        </span>
        <span className="mt-2 flex flex-wrap items-center gap-2">
          <StateBadge state={group.face.state} label={group.face.stateLabel} />
          {group.face.primaryAction ? (
            <span className="min-w-0 truncate text-2xs font-semibold text-muted" title={group.face.primaryAction.label}>
              Next · {group.face.primaryAction.label}
            </span>
          ) : demoteBlockedStamp ? (
            <span className="min-w-0 truncate text-2xs text-faint" title={blockedReasonPlain(group.face.unavailableReason)}>
              Blocked · see banner above
            </span>
          ) : (
            <span className="min-w-0 truncate text-2xs text-seam" title={blockedReasonPlain(group.face.unavailableReason)}>
              Can't act yet · {blockedReasonPlain(group.face.unavailableReason)}
            </span>
          )}
          {group.count > 1 ? <span className="mono text-2xs text-faint">{group.count} items</span> : null}
        </span>
      </span>
    </button>
  );
}

function EvidencePanel({ group }: { group: DecideGroup }) {
  const rows = (
    <div className="divide-y divide-hair border-y border-hair">
      {group.evidence.map((evidence, index) => (
        <div key={`${evidence.ref}:${index}`} className="grid gap-1 py-2.5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-xs font-semibold text-ink">{evidence.label}</span>
            {evidence.sourceKind ? <span className="rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs text-faint">{evidence.sourceKind}</span> : null}
          </div>
          {evidence.summary ? <p className="text-xs leading-relaxed text-muted">{evidence.summary}</p> : null}
          {evidence.ref ? <code className="mono break-all text-2xs text-faint">{evidence.ref}</code> : null}
          {evidence.openCommand ? <div><CopyCmd cmd={evidence.openCommand} compact /></div> : null}
        </div>
      ))}
    </div>
  );

  return (
    <div className="grid gap-3">
      {/*
       * One freshness stamp here, not two: the decision's own proposed-at
       * age already appears once, clearly labeled, in the card header above
       * ("Proposed … (age)"). Repeating a bare, unlabeled age+"stale" badge
       * next to the live/stale FreshnessChip read as two contradictory
       * freshness claims on one row — so only the projection-freshness
       * chip (what this Evidence tab is actually about) lives here.
       */}
      <div className="flex flex-wrap items-center gap-3 rounded-md bg-soft px-3 py-2">
        <code className="mono text-2xs text-faint" title={group.sampleId}>{group.sampleId}</code>
        <span className="inline-flex items-center gap-1.5">
          <span className="text-faint">Queue refreshed</span>
          <FreshnessChip generatedAt={group.face.freshness.generatedAt} source={group.face.freshness.source} />
        </span>
      </div>
      {group.evidence.length ? rows : (
        <p className="rounded-md border border-dashed border-hair px-3 py-6 text-center text-xs text-muted">
          No additional evidence references are projected for this decision.
        </p>
      )}
    </div>
  );
}

function HistoryPanel({ group }: { group: DecideGroup }) {
  return (
    <div className="grid gap-3">
      <ol className="grid gap-2" aria-label="Decision history">
        <li className="grid grid-cols-[auto_1fr] gap-3 rounded-md border border-hair bg-soft px-3 py-2.5">
          <span aria-hidden className="mt-1 h-2 w-2 rounded-full bg-seam" />
          <div>
            <div className="text-xs font-semibold text-ink">Raised for review</div>
            <p className="mt-0.5 text-xs text-muted">
              {group.face.proposedAtLabel ? (
                <><time dateTime={group.face.proposedAt}>{group.face.proposedAtLabel}</time> ({group.ageLabel}).</>
              ) : `Proposal time unavailable (${group.ageLabel}).`}
            </p>
          </div>
        </li>
        <li className="grid grid-cols-[auto_1fr] gap-3 rounded-md border border-hair bg-soft px-3 py-2.5">
          <span aria-hidden className="mt-1 h-2 w-2 rounded-full bg-work" />
          <div>
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-xs font-semibold text-ink">Current owner projection</span>
              <StateBadge state={group.face.state} label={group.face.stateLabel} />
            </div>
            <p className="mt-1 text-xs text-muted">No later receipt or transition is claimed by this face.</p>
          </div>
        </li>
      </ol>
      {group.members.length > 1 ? <ReadonlyMembers members={group.members} atomic={group.act.mode === "batch-atomic"} /> : null}
    </div>
  );
}

function ComparePanel({ group }: { group: DecideGroup }) {
  const preparedOptions = group.brief?.options ?? [];
  const genericPaths = [
    {
      key: "act",
      label: group.face.primaryAction?.label || "Act now",
      consequence: group.consequences?.approve || "",
      tone: "good",
    },
    {
      key: "wait",
      label: "Wait",
      consequence: group.consequences?.hold || "",
      tone: "memory",
    },
  ];
  return (
    <div className="grid gap-4">
      <div>
        <div className="text-2xs font-bold uppercase tracking-wide text-faint">Decision comparison</div>
        <p className="mt-1 text-xs leading-relaxed text-muted">
          Known owner-projected consequences are shown verbatim. Missing consequences stay unknown.
        </p>
      </div>
      {preparedOptions.length ? (
        <div className="grid gap-3 md:grid-cols-2">
          {preparedOptions.map((option) => {
            const recommended = group.brief?.recommendation.option === option.value;
            return <div key={option.value} className={`rounded-lg border px-3 py-3 ${recommended ? "border-good-line bg-good-soft/45" : "border-hair bg-soft"}`}>
              <div className="flex items-center justify-between gap-2"><div className="text-sm font-semibold text-ink">{option.label || option.value}</div>{recommended ? <span className="rounded-pill bg-good-soft px-2 py-0.5 text-2xs font-semibold text-good">prepared recommendation</span> : null}</div>
              <div className="mt-3"><div className="section-label">If chosen</div><p className="mt-1 text-xs leading-relaxed text-ink">{option.consequenceApprove || "Consequence not projected by the owner."}</p></div>
              <div className="mt-3"><div className="section-label">If not chosen</div><p className="mt-1 text-xs leading-relaxed text-muted">{option.consequenceHold || "Consequence not projected by the owner."}</p></div>
            </div>;
          })}
        </div>
      ) : (
        <div className="grid gap-3 md:grid-cols-2">
          {genericPaths.map((path) => <div key={path.key} className={`rounded-xl border px-4 py-4 ${path.tone === "good" ? "border-good-line bg-good-soft/45" : "border-memory-line bg-memory-soft/45"}`}>
            <div className={`text-md font-semibold ${path.tone === "good" ? "text-good" : "text-memory"}`}>{path.label}</div>
            <div className="mt-3 section-label">Projected consequence</div>
            <p className="mt-1 text-sm leading-relaxed text-ink">{path.consequence || "Consequence not projected by the owner."}</p>
          </div>)}
        </div>
      )}
      <div className="grid gap-3 sm:grid-cols-3">
        <div className="rounded-md border border-hair bg-panel px-3 py-2"><div className="section-label">Blast radius</div><div className="mt-1 text-xs font-semibold capitalize text-ink">{group.blastRadius?.replace(/_/g, " ") || "not projected"}</div></div>
        <div className="rounded-md border border-hair bg-panel px-3 py-2"><div className="section-label">Reversibility</div><div className="mt-1 text-xs font-semibold capitalize text-ink">{group.reversibility || "not projected"}</div></div>
        <div className="rounded-md border border-hair bg-panel px-3 py-2"><div className="section-label">Evidence</div><div className="mono mt-1 text-xs text-ink">{group.evidence.length} refs</div></div>
      </div>
      {group.brief?.recommendation.rationale ? <div className="rounded-md border border-hair bg-page px-3 py-2.5"><div className="section-label">Prepared rationale</div><p className="mt-1 text-xs leading-relaxed text-muted">{group.brief.recommendation.rationale}</p></div> : null}
    </div>
  );
}

function decisionIdentity(group: DecideGroup): string {
  const { act } = group;
  if (act.mode === "bespoke") {
    return `${group.key}:${act.mode}:${act.itemId}:${act.expectedRevision}:${act.options.map((option) => option.value).join(",")}`;
  }
  if (act.mode === "generic") return `${group.key}:${act.mode}:${act.itemId}:${act.actionId}:${act.expectedRevision}:${act.actionIntentId ?? ""}:${act.expectedAuthorityRevision ?? ""}:${act.requiresConfirmation}:${act.effectKind}`;
  if (act.mode === "batch-atomic") return `${group.key}:${act.mode}:${act.itemId}:${act.option}:${act.argv?.join(" ") ?? ""}`;
  if (act.mode === "batch-enumerable") {
    return `${group.key}:${act.mode}:${act.representedCount}:${act.unavailableCount}:${act.requiresConfirmation}:${act.requiresRationale}:${act.effectKind}:${act.members.map((member) => `${member.id}:${member.actionId}:${member.expectedRevision}:${member.actionIntentId ?? ""}:${member.expectedAuthorityRevision ?? ""}`).join("|")}`;
  }
  return `${group.key}:${act.mode}:${act.command}`;
}

function DecisionDetail({
  group,
  mutationsEnabled,
  excluded,
  onToggleMember,
  retainedOutcome,
  onExecuted,
}: {
  group: DecideGroup;
  mutationsEnabled: boolean;
  excluded: Set<string>;
  onToggleMember: (id: string) => void;
  retainedOutcome?: ExecuteResult | null;
  onExecuted: (result: ExecuteResult) => void;
}) {
  const { act } = group;
  const [tab, setTab] = useState<"summary" | "compare" | "action" | "history" | "evidence">(
    group.brief?.options.length || group.consequences ? "compare" : group.face.state === "ready" ? "action" : "summary",
  );
  const effectKind = group.capability.kind === "unavailable" ? "apply" : group.capability.kind;
  const reviewResolutionActs = genericReviewResolutionActs(group);
  return (
    <article className="decision-focus overflow-hidden rounded-xl border border-hair bg-panel">
      <header className="px-4 py-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex min-w-0 items-start gap-3">
            <GlyphTile domain={group.domain} glyph={group.glyph} />
            <div className="min-w-0 text-2xs font-bold uppercase text-faint">{group.face.kind}</div>
          </div>
          <StateBadge state={group.face.state} label={group.face.stateLabel} />
        </div>

        {/* 1. WHAT is being decided — the full object text, quoted, first and
         * visually primary. Never gated behind a tab: this must read in the
         * first three seconds regardless of which tab happens to be open. */}
        <blockquote className="mt-3 max-w-read border-l-2 border-work-line pl-3 text-lg font-semibold leading-snug text-ink">
          {group.objectText || "No proposal text is available from the owner projection yet."}
        </blockquote>
        {group.title && group.title !== group.objectText ? (
          <p className="mt-1 max-w-read text-xs font-medium text-muted">{operatorText(group.title)}</p>
        ) : null}

        {/* 2. WHAT changes if approved — one sentence when derivable; the row
         * is simply absent (no placeholder filler) when it is not. */}
        {group.consequences?.approve ? (
          <p className="mt-2 max-w-read text-sm leading-relaxed text-ink">
            <span className="font-semibold text-good">If approved</span>
            <span className="text-faint"> → </span>
            {group.consequences.approve}
          </p>
        ) : null}

        {/* 3. WHAT to do now — the action when the group exposes one,
         * otherwise ONE plain-language blocked line (never the raw jargon
         * reason string verbatim). */}
        <div className="mt-4 grid gap-3 md:grid-cols-2">
          <div className="rounded-md border border-hair bg-soft px-3 py-2.5">
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">What to do now</div>
            {group.face.primaryAction ? (
              <p className="mt-1 text-sm leading-relaxed text-ink">
                <span className="font-semibold text-good">{group.face.primaryAction.label}</span> — open the Action tab below.
              </p>
            ) : (
              <p className="mt-1 text-sm leading-relaxed text-seam">
                {blockedReasonPlain(group.face.unavailableReason) || "No governed action is available yet."}
              </p>
            )}
          </div>

          <div className="rounded-md border border-hair bg-soft px-3 py-2.5">
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">Why now</div>
            <p className="mt-1 max-w-read text-sm leading-relaxed text-ink">{group.face.whyNow}</p>
          </div>
        </div>
        <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2 text-2xs text-faint">
          <span className="inline-flex items-center gap-1.5">
            <span>Proposed</span>
            {group.face.proposedAtLabel ? (
              <time dateTime={group.face.proposedAt} className="font-semibold tabular-nums text-muted">
                {group.face.proposedAtLabel} ({group.ageLabel})
              </time>
            ) : <strong className="font-semibold text-muted">Time unavailable</strong>}
          </span>
          <span className="inline-flex items-center gap-1.5"><span>Owner</span><strong className="font-semibold text-muted">{group.face.owner}</strong></span>
          <span className="inline-flex items-center gap-1.5">
            <span>Queue refreshed</span>
            <FreshnessChip generatedAt={group.face.freshness.generatedAt} source={group.face.freshness.source} />
          </span>
          {group.face.confidence ? <span className="tabular-nums">{group.face.confidence.label}</span> : null}
        </div>
      </header>

      {group.face.authority ? <AuthorityFaceDetails authority={group.face.authority} /> : null}

      <Tabs value={tab} onValueChange={(value) => setTab(value as typeof tab)} className="border-t border-hair">
        <div className="overflow-x-auto px-4 py-3">
          <TabsList label="Decision face">
            <TabsTrigger value="compare" count={group.brief?.options.length}>Compare</TabsTrigger>
            <TabsTrigger value="summary">Summary</TabsTrigger>
            <TabsTrigger value="action">Action</TabsTrigger>
            <TabsTrigger value="history">History</TabsTrigger>
            <TabsTrigger value="evidence" count={group.evidence.length}>Evidence</TabsTrigger>
          </TabsList>
        </div>

        <TabsContent value="summary">
          <section className="border-t border-hair px-4 py-4">
            {group.body ? <p className="max-w-read text-sm leading-relaxed text-muted">{group.body}</p> : (
              <p className="text-sm leading-relaxed text-muted">This decision is waiting at the owner boundary shown above.</p>
            )}
            {group.consequences ? <ConsequencePair approve={group.consequences.approve} hold={group.consequences.hold} /> : null}
            {group.brief ? (
              <details className="group mt-4 border-t border-hair pt-3">
                <summary className="flex cursor-pointer list-none items-center justify-between gap-3 text-xs font-semibold text-muted marker:hidden">
                  <span>Prepared analysis</span>
                  <span aria-hidden className="text-sm text-faint transition-transform group-open:rotate-90">›</span>
                </summary>
                <div className="mt-3"><BriefPanel brief={group.brief} /></div>
              </details>
            ) : null}
          </section>
        </TabsContent>

        <TabsContent value="compare">
          <section className="border-t border-hair px-4 py-4"><ComparePanel group={group} /></section>
        </TabsContent>

        <TabsContent value="action">
          <section className="border-t border-hair bg-soft/60 px-4 py-4">
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
              <div>
                <div className="text-2xs font-bold uppercase tracking-wide text-faint">Primary action</div>
                <div className="mt-0.5 text-sm font-semibold text-ink">{group.face.primaryAction?.label || "No governed owner action yet"}</div>
                {group.face.primaryAction?.description ? (
                  <p className="mt-1 max-w-read text-xs leading-relaxed text-muted">{group.face.primaryAction.description}</p>
                ) : null}
              </div>
              <CapabilityStatus group={group} />
            </div>
            {act.mode === "copy" ? (
              <div className="grid gap-3 border-l-2 border-seam-line pl-3">
                <div>
                  <p className="text-sm font-semibold text-seam">
                    {group.reviewResolutionItemId ? ownerUnavailableMessage(group.area) : blockedReasonPlain(group.face.unavailableReason)}
                  </p>
                  <p className="mt-1 text-xs leading-relaxed text-muted">{group.face.unblockPath}</p>
                </div>
                {reviewResolutionActs.map((resolutionAct) => (
                  <GovernedAct
                    key={resolutionAct.actionId}
                    act={resolutionAct}
                    mutationsEnabled={mutationsEnabled}
                    effectKind="record"
                  />
                ))}
                {!group.authorityBound ? (
                  <details className="text-xs text-muted">
                    <summary className="cursor-pointer font-semibold">Manual owner command</summary>
                    <div className="mt-2"><CopyCmd cmd={act.command} compact /></div>
                    {act.reason ? <p className="mt-2 leading-relaxed text-faint">{act.reason}</p> : null}
                  </details>
                ) : null}
              </div>
            ) : act.mode === "batch-enumerable" ? (
              <EnumerableApply
                area={act.area}
                members={act.members}
                excluded={excluded}
                onToggle={onToggleMember}
                mutationsEnabled={mutationsEnabled}
                requiresConfirmation={act.requiresConfirmation}
                requiresRationale={act.requiresRationale}
                effectKind={act.effectKind}
                policyNote={act.policyNote}
                label={group.face.primaryAction?.label || act.label}
                authorityBound={group.authorityBound === true}
                representedCount={act.representedCount}
                unavailableCount={act.unavailableCount}
              />
            ) : (
              <GovernedAct
                key={decideActIdentity(act)}
                act={act}
                authority={group.face.authority}
                mutationsEnabled={mutationsEnabled}
                onExecuted={onExecuted}
                retainedOutcome={retainedOutcome}
                effectKind={effectKind}
              />
            )}
          </section>
        </TabsContent>

        <TabsContent value="history"><section className="border-t border-hair px-4 py-4"><HistoryPanel group={group} /></section></TabsContent>
        <TabsContent value="evidence"><section className="border-t border-hair px-4 py-4"><EvidencePanel group={group} /></section></TabsContent>
      </Tabs>
    </article>
  );
}

type RetainedDecision = { group: DecideGroup; result: ExecuteResult; identity: string };

export function initialDecideIndex(
  groups: ReadonlyArray<Pick<DecideGroup, "key">>,
  requestedKey = "",
): number {
  const requestedIndex = requestedKey ? groups.findIndex((group) => group.key === requestedKey) : -1;
  return requestedIndex >= 0 ? requestedIndex : groups.length > 0 ? 0 : -1;
}

export function initialDecideKey(
  groups: ReadonlyArray<Pick<DecideGroup, "key">>,
  requestedKey = "",
): string {
  const index = initialDecideIndex(groups, requestedKey);
  return index >= 0 ? groups[index]?.key ?? "" : "";
}

export function decisionSelectionHref(pathname: string, currentSearch: string, decisionKey: string): string {
  const params = new URLSearchParams(currentSearch);
  if (decisionKey) params.set("decision", decisionKey);
  else params.delete("decision");
  const query = params.toString();
  return query ? `${pathname}?${query}` : pathname;
}

export function DecideQueue({
  groups,
  mutationsEnabled,
  keyboardShortcuts = true,
  initialSelectedKey = "",
  systemicBlockerReason = null,
}: {
  groups: DecideGroup[];
  mutationsEnabled: boolean;
  keyboardShortcuts?: boolean;
  initialSelectedKey?: string;
  /** Feature-detected queue-level reason (a `systemic_blocker` projection
   * field may land separately); the all-identical-reasons case is always
   * computed from `groups` too, so the banner works even when this is null. */
  systemicBlockerReason?: string | null;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const [filter, setFilter] = useState("");
  const [focused, setFocused] = useState(() => Math.max(0, initialDecideIndex(groups, initialSelectedKey)));
  const [selectedKey, setSelectedKey] = useState(() => initialDecideKey(groups, initialSelectedKey));
  const [excluded, setExcluded] = useState<Record<string, Set<string>>>({});
  const [retained, setRetained] = useState<Record<string, RetainedDecision>>({});

  const filterRef = useRef<HTMLInputElement | null>(null);
  const rowRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const listRef = useRef<HTMLDivElement | null>(null);
  const detailRef = useRef<HTMLDivElement | null>(null);

  const replaceSelectionInURL = useCallback((key: string) => {
    if (!key || searchParams.get("decision") === key) return;
    router.replace(decisionSelectionHref(pathname, searchParams.toString(), key), { scroll: false });
  }, [pathname, router, searchParams]);

  const visible = useMemo(() => {
    const query = filter.trim().toLowerCase();
    if (!query) return groups;
    return groups.filter((group) => `${group.title} ${group.areaLabel} ${group.body}`.toLowerCase().includes(query));
  }, [groups, filter]);
  const readinessSections = useMemo(() => decideReadinessSections(visible), [visible]);
  const visibleIndexByKey = useMemo(() => new Map(visible.map((group, index) => [group.key, index])), [visible]);
  // The whole queue's decidability, not just the filtered view — a search
  // shouldn't change whether the systemic-blocker fact is true.
  const systemicBlocker: SystemicBlocker | null = useMemo(
    () => queueSystemicBlocker(groups, systemicBlockerReason),
    [groups, systemicBlockerReason],
  );

  useEffect(() => {
    const selectedIsCurrent = groups.some((group) => group.key === selectedKey);
    const selectedIsVisible = visible.some((group) => group.key === selectedKey);
    if (!selectedKey && visible[0]) {
      setSelectedKey(visible[0].key);
      replaceSelectionInURL(visible[0].key);
    } else if (selectedIsCurrent && !selectedIsVisible) {
      const nextKey = visible[0]?.key ?? "";
      setSelectedKey(nextKey);
      replaceSelectionInURL(nextKey);
    } else if (selectedKey && !selectedIsCurrent && !retained[selectedKey]) {
      const nextKey = visible[0]?.key ?? "";
      setSelectedKey(nextKey);
      replaceSelectionInURL(nextKey);
    }
    if (focused > visible.length - 1) setFocused(Math.max(0, visible.length - 1));
  }, [focused, groups, replaceSelectionInURL, retained, selectedKey, visible]);

  const requestedIndex = initialDecideIndex(groups, initialSelectedKey);
  const requestedExists = Boolean(
    initialSelectedKey && requestedIndex >= 0 && groups[requestedIndex]?.key === initialSelectedKey,
  );

  useEffect(() => {
    if (!requestedExists) return;
    setSelectedKey(initialSelectedKey);
    setFocused(requestedIndex);
    const timer = window.setTimeout(() => {
      if (!window.matchMedia("(max-width: 1023px)").matches) return;
      rowRefs.current[requestedIndex]?.scrollIntoView({ behavior: "auto", block: "nearest" });
      detailRef.current?.scrollIntoView({ behavior: "auto", block: "start" });
    }, 0);
    return () => window.clearTimeout(timer);
  }, [initialSelectedKey, requestedExists, requestedIndex]);

  useEffect(() => {
    if (!requestedExists && selectedKey) replaceSelectionInURL(selectedKey);
  }, [replaceSelectionInURL, requestedExists, selectedKey]);

  useEffect(() => {
    if (!keyboardShortcuts) return;
    function onKey(event: KeyboardEvent) {
      const element = event.target as HTMLElement | null;
      const typing = Boolean(element?.closest("input, textarea, select, [contenteditable=true]"));
      if (event.key === "/" && !typing) {
        event.preventDefault();
        filterRef.current?.focus();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [keyboardShortcuts]);

  const focusRow = useCallback((index: number) => {
    const clamped = Math.max(0, Math.min(index, rowRefs.current.length - 1));
    setFocused(clamped);
    rowRefs.current[clamped]?.focus();
  }, []);

  const select = useCallback((group: DecideGroup, index: number, scroll = false) => {
    setSelectedKey(group.key);
    setFocused(index);
    replaceSelectionInURL(group.key);
    if (scroll && window.matchMedia("(max-width: 1023px)").matches) {
      const behavior = window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth";
      window.setTimeout(() => detailRef.current?.scrollIntoView({ behavior, block: "start" }), 0);
    }
  }, [replaceSelectionInURL]);

  function onRowKey(event: React.KeyboardEvent<HTMLButtonElement>, index: number, group: DecideGroup) {
    if (event.key === "j" || event.key === "ArrowDown") {
      event.preventDefault();
      focusRow(index + 1);
    } else if (event.key === "k" || event.key === "ArrowUp") {
      event.preventDefault();
      focusRow(index - 1);
    } else if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      select(group, index, true);
    }
  }

  const selected = visible.find((group) => group.key === selectedKey) ?? retained[selectedKey]?.group ?? visible[0] ?? null;
  const liveMessage = selected ? `Selected decision: ${operatorText(selected.title)}` : "Nothing waits on you.";
  const selectedIdentity = selected ? decisionIdentity(selected) : "";
  const selectedRetained = selected ? retained[selected.key] : undefined;
  const panelGroups = groups.filter((group) => Boolean(group.panel));

  if (selected?.panel) {
    return (
      <PanelProofContinuity
        group={selected}
        panelGroups={panelGroups}
        totalDecisions={groups.length}
        mutationsEnabled={mutationsEnabled}
        onSelectGroup={(next) => {
          const nextIndex = Math.max(0, groups.findIndex((group) => group.key === next.key));
          setSelectedKey(next.key);
          setFocused(nextIndex);
          replaceSelectionInURL(next.key);
        }}
        onBack={() => {
          const next = groups.find((group) => !group.panel) ?? groups[0];
          if (!next) return;
          const nextIndex = Math.max(0, groups.findIndex((group) => group.key === next.key));
          setSelectedKey(next.key);
          setFocused(nextIndex);
          replaceSelectionInURL(next.key);
        }}
      />
    );
  }

  return (
    <div className="grid gap-3">
      <div aria-live="polite" className="sr-only">{liveMessage}</div>
      {systemicBlocker ? (
        <div role="status" className="rounded-lg border border-seam-line bg-seam-soft px-4 py-3">
          <div className="text-sm font-semibold text-seam">
            {systemicBlocker.totalCount - systemicBlocker.blockedCount} of {systemicBlocker.totalCount} decidable
          </div>
          <p className="mt-1 text-xs leading-relaxed text-muted">{blockedReasonPlain(systemicBlocker.reason)}</p>
        </div>
      ) : null}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <input
          ref={filterRef}
          name="decision-filter"
          autoComplete="off"
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
          placeholder="Filter decisions…"
          aria-label="Filter decisions"
          className="w-full rounded-md border border-hair bg-panel px-3 py-2 text-sm text-ink outline-none focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus sm:max-w-xs"
        />
        <span className="mono text-2xs tabular-nums text-faint">{visible.length} decisions</span>
      </div>

      <div className="grid items-start gap-4 lg:grid-cols-5">
        <div ref={listRef} className="scroll-mt-16 max-h-[52vh] overflow-y-auto rounded-lg border border-hair bg-panel lg:col-span-2 lg:max-h-none lg:overflow-visible">
          {visible.length ? readinessSections.map((section) => (
            <section
              key={section.key}
              aria-label={section.key === "act_now" ? "Act now decisions" : "Needs context decisions"}
              className="border-b border-hair last:border-b-0"
            >
              <div className={`px-3 py-2.5 ${section.key === "act_now" ? "bg-work-soft" : "bg-seam-soft"}`}>
                <div className="flex items-center justify-between gap-2">
                  <h3 className="text-xs font-bold uppercase tracking-wide text-ink">{section.label}</h3>
                  <span className="mono rounded-pill border border-hair bg-panel px-2 py-0.5 text-2xs text-muted">{section.groups.length}</span>
                </div>
                <p className="mt-0.5 text-2xs text-muted">{section.description}</p>
              </div>
              {section.groups.map((group) => {
                const index = visibleIndexByKey.get(group.key) ?? 0;
                return (
                  <DecisionRow
                    key={group.key}
                    group={group}
                    selected={selected?.key === group.key}
                    demoteBlockedStamp={Boolean(systemicBlocker)}
                    rowRef={(element) => { rowRefs.current[index] = element; }}
                    onActivate={() => select(group, index, true)}
                    onFocus={() => select(group, index)}
                    onKeyDown={(event) => onRowKey(event, index, group)}
                  />
                );
              })}
            </section>
          )) : (
            <p className="border-dashed px-4 py-8 text-center text-sm text-muted">No decision matches “{filter}”.</p>
          )}
        </div>

        <div ref={detailRef} className="scroll-mt-20 lg:sticky lg:top-20 lg:col-span-3">
          {selected ? (
            <div className="grid gap-2">
              <button
                type="button"
                onClick={() => {
                  const behavior = window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth";
                  listRef.current?.scrollIntoView({ behavior, block: "start" });
                }}
                className="inline-flex min-h-11 w-fit items-center gap-1.5 rounded-md border border-hair bg-panel px-3 py-2 text-xs font-semibold text-muted hover:bg-soft lg:hidden"
              >
                <span aria-hidden>↑</span>
                All decisions
              </button>
              <DecisionDetail
                key={selectedIdentity}
                group={selected}
                mutationsEnabled={mutationsEnabled}
                excluded={excluded[selected.key] ?? new Set()}
                onToggleMember={(id) => setExcluded((previous) => {
                  const next = new Set(previous[selected.key] ?? []);
                  if (next.has(id)) next.delete(id); else next.add(id);
                  return { ...previous, [selected.key]: next };
                })}
                retainedOutcome={selectedRetained?.identity === selectedIdentity ? selectedRetained.result : null}
                onExecuted={(result) => setRetained((previous) => ({
                  ...previous,
                  [selected.key]: { group: selected, result, identity: selectedIdentity },
                }))}
              />
            </div>
          ) : (
            <div className="rounded-lg border border-dashed border-hair bg-page px-4 py-10 text-center text-sm text-muted">Clean seam. Nothing waits on you.</div>
          )}
        </div>
      </div>
    </div>
  );
}
