"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { Button, FreshnessChip, StateBadge } from "@/components/kit";
import type { DecideAct, DecideGroup, PanelReview } from "@/lib/decide-model";
import { blockedReasonPlain } from "@/lib/glossary";
import { GovernedAct } from "./GovernedAct";

type StageStatus = "done" | "current" | "ready" | "blocked" | "unknown" | "absent";

type ProofStage = {
  key: string;
  label: string;
  status: StageStatus;
  statusLabel: string;
  description: string;
  meta: string;
};

const STATUS_CLASS: Record<StageStatus, string> = {
  done: "border-good-line bg-good-soft text-good",
  current: "border-work-line bg-work-soft text-work",
  ready: "border-good-line bg-good-soft text-good",
  blocked: "border-seam-line bg-seam-soft text-seam",
  unknown: "border-seam-line bg-seam-soft text-seam",
  absent: "border-hair bg-soft text-faint",
};

const FINDING_CLASS: Record<string, string> = {
  high: "border-crit-line bg-crit-soft text-crit",
  medium: "border-seam-line bg-seam-soft text-seam",
  low: "border-hair bg-soft text-muted",
};

const DISPOSITION_ORDER = ["needs_human", "park", "promote", "reject", "no_promote"];

function dispositionLabel(value: string): string {
  if (value === "needs_human") return "Needs human";
  if (value === "no_promote") return "No-promote";
  return value
    .replace(/_/g, " ")
    .replace(/\b\w/g, (letter) => letter.toUpperCase());
}

function shortId(value: string, head = 16): string {
  if (value.length <= head + 5) return value;
  return `${value.slice(0, head)}…${value.slice(-4)}`;
}

function stageNumber(index: number): string {
  return String(index + 1);
}

function recordAct(review: PanelReview): Extract<DecideAct, { mode: "generic" }> | null {
  const member = review.member;
  if (
    !member.actionId ||
    !member.argv?.length ||
    !member.expectedRevision ||
    !member.actionIntentId ||
    !member.expectedAuthorityRevision
  ) {
    return null;
  }
  return {
    mode: "generic",
    area: "panel-verdicts",
    itemId: review.id,
    actionId: member.actionId,
    expectedRevision: member.expectedRevision,
    actionIntentId: member.actionIntentId,
    expectedAuthorityRevision: member.expectedAuthorityRevision,
    label: member.actionLabel || "Record human review",
    argv: member.argv,
    requiresConfirmation: true,
    copyCommand: `compound ${member.argv.join(" ")}`,
    effectKind: "record",
    effectDescription:
      member.policyNote ||
      "Records the human-review sidecar only. The selected memory adjudication remains a separate explicit act.",
    reasonRequired: true,
  };
}

function proofStages(group: DecideGroup, review: PanelReview): ProofStage[] {
  const recordAuthority = review.member.authority;
  const recordReady = Boolean(
    group.face.state === "ready" &&
    recordAuthority?.current &&
    recordAuthority.allowed &&
    recordAct(review),
  );
  const policyProjected = Boolean(recordAuthority?.policyDecisionId);
  const lensCount = review.findings.length;
  return [
    {
      key: "observed",
      label: "Observed candidate",
      status: "done",
      statusLabel: "Completed",
      description: review.sourceRef
        ? "A source-backed candidate exists and remains pre-authority."
        : "A candidate exists; a direct source reference is not projected.",
      meta: review.proposedAtLabel,
    },
    {
      key: "lenses",
      label: "Lens review",
      status: "done",
      statusLabel: "Completed",
      description: `${lensCount} lens finding${lensCount === 1 ? "" : "s"} are projected for this claim.`,
      meta: review.runId,
    },
    {
      key: "synthesis",
      label: "Judge synthesis",
      status: "done",
      statusLabel: "Completed",
      description: review.panelLean
        ? `Panel lean: ${review.panelLean.replace(/_/g, " ")}.`
        : "A verdict artifact exists; no lean label is projected.",
      meta: review.verdictId,
    },
    {
      key: "human",
      label: "Human review",
      status: "current",
      statusLabel: "Current",
      description: "Operator choice is required. No selection has been written by the console.",
      meta: review.optionalRationale ? "Rationale accepted" : "Rationale policy not projected",
    },
    {
      key: "action",
      label: "Governed action",
      status: recordReady ? "ready" : "blocked",
      statusLabel: recordReady ? "Ready" : "Blocked",
      description: recordReady
        ? "A governed review-record action is ready; it does not adjudicate memory."
        : blockedReasonPlain(group.face.unavailableReason) ||
          "Current action authority is not available to the console.",
      meta: review.authorityWorkId || "No authority work id projected",
    },
    {
      key: "policy",
      label: "PolicyDecision",
      status: recordReady ? "done" : policyProjected ? "blocked" : "unknown",
      statusLabel: recordReady ? "Current" : policyProjected ? "Not actionable" : "Unknown",
      description: recordReady
        ? recordAuthority?.wording || "Current policy allows the review-record action with obligations."
        : policyProjected
          ? "A policy decision is projected for the review action, but current Decide freshness does not authorize use."
          : "No current PolicyDecision is projected for a console action.",
      meta: recordAuthority?.policyDecisionId || "",
    },
    {
      key: "receipt",
      label: "Receipt",
      status: "absent",
      statusLabel: "Not created",
      description: "No receipt is projected for this human choice.",
      meta: "",
    },
    {
      key: "transition",
      label: "Transition",
      status: "absent",
      statusLabel: "Not created",
      description: "No transition is projected for this human choice.",
      meta: "",
    },
  ];
}

function StageCard({
  stage,
  index,
  selected,
  onSelect,
}: {
  stage: ProofStage;
  index: number;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={onSelect}
      className={`min-w-0 rounded-md border p-2.5 text-left outline-none transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus ${
        selected ? "border-work-line bg-work-soft" : "border-hair bg-panel hover:bg-soft"
      }`}
    >
      <span className="flex items-start gap-2">
        <span
          aria-hidden
          className={`mono inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-pill border text-2xs font-bold ${STATUS_CLASS[stage.status]}`}
        >
          {stageNumber(index)}
        </span>
        <span className="min-w-0">
          <span className="block text-xs font-semibold leading-snug text-ink">{stage.label}</span>
          <span className={`mt-1 block text-2xs font-bold uppercase tracking-wide ${STATUS_CLASS[stage.status].split(" ").at(-1)}`}>
            {stage.statusLabel}
          </span>
        </span>
      </span>
      <span className="mt-2.5 line-clamp-2 block text-2xs leading-relaxed text-muted">{stage.description}</span>
      {stage.meta ? (
        <span className="mono mt-2 block truncate text-2xs text-faint" title={stage.meta}>
          {stage.meta}
        </span>
      ) : null}
    </button>
  );
}

function FindingRow({ finding }: { finding: PanelReview["findings"][number] }) {
  const tone = FINDING_CLASS[finding.severity] ?? FINDING_CLASS.low;
  return (
    <li className="grid grid-cols-[auto_1fr] gap-2 border-b border-hair py-2 last:border-b-0">
      <span className={`mt-0.5 rounded-pill border px-2 py-0.5 text-2xs font-semibold capitalize ${tone}`}>
        {finding.severity || "unknown"}
      </span>
      <span className="min-w-0">
        <span className="block text-xs font-semibold capitalize text-ink">{finding.label}</span>
        {finding.question ? <span className="mt-0.5 block text-2xs text-faint">{finding.question}</span> : null}
        <span className="mt-1 line-clamp-2 block text-xs leading-relaxed text-muted">{finding.detail}</span>
      </span>
    </li>
  );
}

export function PanelProofContinuity({
  group,
  panelGroups,
  totalDecisions,
  mutationsEnabled,
  onSelectGroup,
  onBack,
}: {
  group: DecideGroup;
  panelGroups: DecideGroup[];
  totalDecisions: number;
  mutationsEnabled: boolean;
  onSelectGroup: (group: DecideGroup) => void;
  onBack: () => void;
}) {
  const panel = group.panel;
  const [reviewId, setReviewId] = useState(panel?.reviews[0]?.id ?? "");
  const [selectedChoice, setSelectedChoice] = useState("");
  const [selectedStage, setSelectedStage] = useState(3);

  useEffect(() => {
    setReviewId(panel?.reviews[0]?.id ?? "");
    setSelectedChoice("");
    setSelectedStage(3);
  }, [group.key, panel?.reviews]);

  const review = panel?.reviews.find((candidate) => candidate.id === reviewId) ?? panel?.reviews[0] ?? null;
  const runs = useMemo(
    () => [...new Set(panelGroups.map((candidate) => candidate.panel?.runId).filter(Boolean) as string[])],
    [panelGroups],
  );
  const currentRun = panel?.runId ?? "";
  const currentRunGroups = useMemo(
    () =>
      panelGroups
        .filter((candidate) => candidate.panel?.runId === currentRun)
        .sort(
          (left, right) =>
            DISPOSITION_ORDER.indexOf(left.panel?.disposition ?? "") -
            DISPOSITION_ORDER.indexOf(right.panel?.disposition ?? ""),
        ),
    [currentRun, panelGroups],
  );

  if (!panel || !review) {
    return (
      <div className="rounded-lg border border-dashed border-hair bg-page px-4 py-10 text-center text-sm text-muted">
        Panel decision detail is not projected for this group.
      </div>
    );
  }

  const panelReviews = panel.reviews;
  const reviewIndex = panelReviews.findIndex((candidate) => candidate.id === review.id);
  const stages = proofStages(group, review);
  const stage = stages[selectedStage] ?? stages[3];
  const selected = review.choices.find((choice) => choice.id === selectedChoice) ?? null;
  const highSignalFindings = review.findings
    .slice()
    .sort((left, right) => {
      const rank = { high: 0, medium: 1, low: 2 };
      return (rank[left.severity as keyof typeof rank] ?? 3) - (rank[right.severity as keyof typeof rank] ?? 3);
    })
    .slice(0, 3);
  const safeRecordAct =
    panel.projectionAligned && group.face.state === "ready" ? recordAct(review) : null;
  const safeRecordAuthority = safeRecordAct ? review.member.authority ?? null : null;

  function selectRun(runId: string) {
    const next =
      panelGroups.find(
        (candidate) =>
          candidate.panel?.runId === runId && candidate.panel?.disposition === "needs_human",
      ) ?? panelGroups.find((candidate) => candidate.panel?.runId === runId);
    if (next) onSelectGroup(next);
  }

  function moveReview(delta: number) {
    const nextIndex = (reviewIndex + delta + panelReviews.length) % panelReviews.length;
    const next = panelReviews[nextIndex];
    if (next) {
      setReviewId(next.id);
      setSelectedChoice("");
      setSelectedStage(3);
    }
  }

  return (
    <article className="overflow-hidden rounded-xl border border-hair bg-panel shadow-sm">
      <div className="flex min-h-12 items-stretch overflow-x-auto border-b border-hair bg-soft">
        <div className="flex min-w-fit items-center px-2">
          <Button variant="secondary" onClick={onBack}>
            All decisions · {totalDecisions}
          </Button>
        </div>
        <label className="flex min-w-[22rem] items-center gap-2 border-l border-hair px-3">
          <span className="text-2xs font-bold uppercase text-faint">Run</span>
          <select
            value={currentRun}
            onChange={(event) => selectRun(event.target.value)}
            aria-label="Panel run"
            className="mono min-w-0 flex-1 rounded-md border border-hair bg-panel px-2 py-1.5 text-xs text-muted outline-none focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
          >
            {runs.map((runId) => (
              <option key={runId} value={runId}>
                {runId}
              </option>
            ))}
          </select>
        </label>
        <div className="flex min-w-fit" aria-label="Panel verdict groups">
          {currentRunGroups.map((candidate) => {
            const active = candidate.key === group.key;
            const disposition = candidate.panel?.disposition ?? "unknown";
            return (
              <button
                key={candidate.key}
                type="button"
                aria-pressed={active}
                onClick={() => onSelectGroup(candidate)}
                className={`flex min-w-fit items-center gap-2 border-l border-hair px-3 text-xs font-semibold outline-none focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus ${
                  active ? "bg-seam-soft text-seam" : "bg-panel text-muted hover:bg-soft"
                }`}
              >
                {dispositionLabel(disposition)}
                <span className="mono rounded-pill border border-hair bg-panel px-2 py-0.5 text-2xs tabular-nums text-muted">
                  {candidate.count}
                </span>
              </button>
            );
          })}
        </div>
        <span className="ml-auto flex min-w-fit items-center border-l border-hair px-3 text-2xs text-faint">
          {runs.length > 1 ? `${runs.length - 1} other run${runs.length === 2 ? "" : "s"} in review` : "One run in review"}
        </span>
      </div>

      <header className="grid gap-4 border-b border-hair px-5 py-3.5 lg:grid-cols-[1fr_21rem]">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-2xs font-bold uppercase tracking-wide text-faint">Claim under review</span>
            <span className="flex items-center gap-2">
              <Button variant="secondary" onClick={() => moveReview(-1)} disabled={panelReviews.length < 2}>
                Previous
              </Button>
              <span className="mono text-2xs tabular-nums text-faint">
                {reviewIndex + 1} / {panelReviews.length}
              </span>
              <Button variant="secondary" onClick={() => moveReview(1)} disabled={panelReviews.length < 2}>
                Next
              </Button>
            </span>
          </div>
          <h2 className="mt-1.5 max-w-read text-xl font-semibold leading-tight text-ink">{review.claim}</h2>
          {review.sourceText ? (
            <p className="mt-1.5 max-w-read text-xs leading-relaxed text-muted">
              <span className="font-semibold text-faint">Source text</span> · {review.sourceText}
            </p>
          ) : null}
          <div className="mono mt-2 flex flex-wrap gap-x-3 gap-y-1 text-2xs text-faint">
            <span title={review.runId}>run {shortId(review.runId, 22)}</span>
            {review.targetKind ? <span>{review.targetKind.replace(/_/g, " ")}</span> : null}
            {review.targetRef ? <span title={review.targetRef}>{shortId(review.targetRef)}</span> : null}
            {review.panelLean ? <span>{review.panelLean.replace(/_/g, " ")}</span> : null}
            <span>raised {group.ageLabel}</span>
          </div>
        </div>

        <aside className="border-l-2 border-seam-line pl-4">
          <div className="text-sm font-semibold text-seam">Panel verdict is evidence, not authority.</div>
          <p className="mt-2 text-xs leading-relaxed text-muted">
            Human review and a current PolicyDecision still govern every mutation. The run does not decide or act by itself.
          </p>
          {!panel.projectionAligned ? (
            <p className="mt-2 rounded-md border border-seam-line bg-seam-soft px-2.5 py-2 text-2xs leading-relaxed text-seam">
              Review detail is retained from the matching run while Attention membership refreshes. It remains evidence-only; actions stay paused until both projections name the same members.
            </p>
          ) : null}
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <StateBadge state={group.face.state} label={group.face.stateLabel} />
            <FreshnessChip
              generatedAt={group.face.freshness.generatedAt}
              source={group.face.freshness.source}
            />
          </div>
          {review.authorityWorkId ? (
            <Link
              href={`/work?trace=${encodeURIComponent(review.authorityWorkId)}`}
              className="mt-3 inline-flex rounded-md border border-hair bg-panel px-3 py-2 text-xs font-semibold text-work hover:bg-soft"
            >
              Open review authority work
            </Link>
          ) : (
            <p className="mt-3 text-xs text-faint">No run-bound WorkEnvelope is projected.</p>
          )}
        </aside>
      </header>

      <section className="border-b border-hair px-5 py-3.5" aria-label="Proof continuity trace">
        <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4 xl:grid-cols-8">
          {stages.map((item, index) => (
            <StageCard
              key={item.key}
              stage={item}
              index={index}
              selected={selectedStage === index}
              onSelect={() => setSelectedStage(index)}
            />
          ))}
        </div>
        <div className="sr-only" aria-live="polite">
          {stage.label}. {stage.statusLabel}. {stage.description}
        </div>
      </section>

      <section className="grid gap-5 px-5 py-3 lg:grid-cols-[1.2fr_0.85fr_1.25fr]">
        <div className="min-w-0">
          <h3 className="border-l-2 border-work-line pl-3 text-md font-semibold text-ink">
            Human review <span className="font-normal text-muted">— operator choice required</span>
          </h3>
          <div className="mt-4 text-2xs font-bold uppercase tracking-wide text-faint">Synthesis rationale</div>
          <p className="mt-2 max-w-read text-sm leading-relaxed text-muted">
            {review.rationale || "No panel synthesis rationale is projected for this claim."}
          </p>

          <div className="mt-4 border-t border-hair pt-3">
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">Claim evidence</div>
            <div className="mt-2 flex flex-wrap gap-2">
              {review.sourceRef ? (
                <code className="mono rounded-pill border border-hair bg-soft px-2 py-1 text-2xs text-muted">
                  {review.sourceRef}
                </code>
              ) : null}
              {review.evidenceRefs.slice(0, review.sourceRef ? 2 : 3).map((ref) => (
                <code
                  key={ref}
                  title={ref}
                  className="mono max-w-full truncate rounded-pill border border-hair bg-soft px-2 py-1 text-2xs text-faint"
                >
                  {ref}
                </code>
              ))}
              {review.evidenceRefs.length > (review.sourceRef ? 2 : 3) ? (
                <span className="rounded-pill border border-hair bg-soft px-2 py-1 text-2xs text-faint">
                  +{review.evidenceRefs.length - (review.sourceRef ? 2 : 3)} in Technical depth
                </span>
              ) : null}
            </div>
            {!review.evidenceRefs.length && !review.sourceRef ? (
              <p className="mt-2 text-xs text-faint">No evidence references are projected for this claim.</p>
            ) : null}
          </div>
        </div>

        <div className="min-w-0 border-l border-hair pl-5">
          <div className="text-2xs font-bold uppercase tracking-wide text-faint">Dissent and lens findings</div>
          <ul className="mt-2">
            {highSignalFindings.length ? (
              highSignalFindings.map((finding, index) => (
                <FindingRow key={`${finding.label}:${index}`} finding={finding} />
              ))
            ) : (
              <li className="text-xs text-faint">No lens findings are projected.</li>
            )}
          </ul>
          {review.findings.length > highSignalFindings.length ? (
            <details className="mt-3 border-t border-hair pt-3">
              <summary className="cursor-pointer text-xs font-semibold text-muted">
                All lens findings · {review.findings.length}
              </summary>
              <ul className="mt-2">
                {review.findings.map((finding, index) => (
                  <FindingRow key={`${finding.label}:all:${index}`} finding={finding} />
                ))}
              </ul>
            </details>
          ) : null}
        </div>

        <div className="min-w-0 border-l border-hair pl-5">
          <fieldset>
            <legend className="text-2xs font-bold uppercase tracking-wide text-faint">Operator choice</legend>
            <div className="mt-2 grid gap-1.5">
              {review.choices.map((choice) => {
                const isSelected = selectedChoice === choice.id;
                return (
                  <label
                    key={choice.id}
                    className={`grid cursor-pointer grid-cols-[auto_1fr] gap-x-3 rounded-md border px-3 py-1.5 ${
                      isSelected ? "border-work-line bg-work-soft" : "border-hair bg-panel hover:bg-soft"
                    }`}
                  >
                    <input
                      type="radio"
                      name={`panel-choice-${review.id}`}
                      value={choice.id}
                      checked={isSelected}
                      onChange={() => setSelectedChoice(choice.id)}
                      className="mt-0.5 h-4 w-4 accent-current"
                    />
                    <span>
                      <span className="block text-sm font-semibold text-ink">{choice.label}</span>
                      <span className="mt-0.5 line-clamp-2 block text-2xs leading-snug text-muted" title={choice.effect}>
                        {choice.effect || "No effect description is projected."}
                      </span>
                    </span>
                  </label>
                );
              })}
            </div>
          </fieldset>

          <Button
            variant="primary"
            disabled
            className="mt-3 w-full justify-center"
            title="The backend projects a canonical composite command, but the console does not yet expose a safe combined review-and-adjudicate adapter."
          >
            {selected ? `Apply ${selected.label}` : "Select a decision"}
          </Button>
          <p className="mt-2 text-center text-2xs leading-relaxed text-seam">
            {safeRecordAct
              ? "The selected memory adjudication is not wired as one safe console act. Nothing will be implied or half-applied."
              : `${blockedReasonPlain(group.face.unavailableReason) || "Refresh current authority before acting."} The combined review-and-adjudication act is also not wired.`}
          </p>

          {safeRecordAct && safeRecordAuthority ? (
            <details className="group mt-3 overflow-hidden rounded-md border border-good-line bg-good-soft">
              <summary className="flex cursor-pointer list-none items-center gap-2 px-3 py-2.5 marker:hidden">
                <span aria-hidden className="text-sm text-good transition-transform group-open:rotate-90">›</span>
                <span className="text-xs font-semibold text-good">Safe review recording available</span>
                <span className="ml-auto text-2xs text-muted">review evidence only</span>
              </summary>
              <div className="border-t border-good-line bg-panel p-3">
                <GovernedAct
                  act={safeRecordAct}
                  authority={safeRecordAuthority}
                  mutationsEnabled={mutationsEnabled}
                  effectKind="record"
                />
              </div>
            </details>
          ) : null}
        </div>
      </section>

      <details className="group border-t border-hair bg-soft">
        <summary className="flex cursor-pointer list-none items-center gap-3 px-5 py-3 marker:hidden">
          <span aria-hidden className="text-sm text-faint transition-transform group-open:rotate-90">›</span>
          <span className="text-xs font-semibold text-ink">Technical depth</span>
          <span className="text-xs text-faint">collapsed</span>
          <span className="mono ml-auto truncate text-2xs text-faint" title={review.runId}>
            {review.runId}
          </span>
        </summary>
        <div className="grid gap-4 border-t border-hair px-5 py-4 text-xs md:grid-cols-2">
          <div>
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">Identifiers</div>
            <dl className="mt-2 grid grid-cols-[7rem_1fr] gap-x-3 gap-y-1.5">
              <dt className="text-faint">Run</dt>
              <dd className="mono break-all text-muted">{review.runId}</dd>
              <dt className="text-faint">Verdict</dt>
              <dd className="mono break-all text-muted">{review.verdictId}</dd>
              <dt className="text-faint">Target</dt>
              <dd className="mono break-all text-muted">{review.targetRef || "not projected"}</dd>
              <dt className="text-faint">Authority work</dt>
              <dd className="mono break-all text-muted">{review.authorityWorkId || "not projected"}</dd>
            </dl>
          </div>
          <div>
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">Owner boundary</div>
            {review.boundaries.length ? (
              <ul className="mt-2 grid list-disc gap-1.5 pl-4">
                {review.boundaries.map((boundary) => (
                  <li key={boundary} className="text-muted">{boundary}</li>
                ))}
              </ul>
            ) : (
              <p className="mt-2 text-faint">No boundary statements are projected.</p>
            )}
            {review.canonicalActionHint ? (
              <p className="mt-3 border-t border-hair pt-3 leading-relaxed text-muted">{review.canonicalActionHint}</p>
            ) : null}
          </div>
          <div>
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">All evidence references</div>
            <ul className="mono mt-2 grid gap-1.5">
              {[review.sourceRef, ...review.evidenceRefs].filter(Boolean).map((ref) => (
                <li key={ref} className="break-all text-muted">{ref}</li>
              ))}
            </ul>
          </div>
          <div>
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">All lens findings</div>
            <ul className="mt-2">
              {review.findings.map((finding, index) => (
                <FindingRow key={`${finding.label}:technical:${index}`} finding={finding} />
              ))}
            </ul>
          </div>
        </div>
      </details>
    </article>
  );
}
