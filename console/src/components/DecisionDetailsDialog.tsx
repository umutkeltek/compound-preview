"use client";

import { useEffect, useMemo, useState } from "react";

import { Chip } from "@/components/ui";
import type { DecisionFormSpec, PlainLanguage } from "@/lib/decision-registry";

type Json = Record<string, unknown>;
type DetailTab = "claim" | "logic" | "source" | "raw";
type LoadedDocument = {
  ok: boolean;
  kind?: string;
  title?: string;
  content?: string;
  truncated?: boolean;
  stderr?: string;
  metadata?: unknown;
};
type DocumentTarget = {
  key: string;
  label: string;
  kind: "path" | "evidence";
  value: string;
};

const PROMOTION_BASES = [
  ["explicit_confirmation", "You personally confirm this should become memory."],
  ["curated_page", "The claim already exists in a curated source page."],
  ["decision_record", "A recorded decision backs the claim."],
  ["runtime_proof", "A real run, test, receipt, or state delta proves it."],
  ["trusted_deterministic_import", "A trusted deterministic importer produced it."],
];

function arr(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function scalar(value: unknown): string {
  if (value == null || value === "") return "—";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return JSON.stringify(value, null, 2);
}

function isObject(value: unknown): value is Json {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function evidenceRef(value: unknown): string {
  const s = str(value).trim();
  return /^ev_[a-z0-9]+$/.test(s) ? s : "";
}

function compactLabel(value: string): string {
  if (value.length <= 56) return value;
  return `${value.slice(0, 28)}…${value.slice(-22)}`;
}

/**
 * The trust feature (spec §6): "What this will NOT do". Boundaries live in
 * different spots depending on the writer — top-level `boundaries`/`not_to_do`
 * on some areas (external-repo-intake), nested `decision_card.boundary` on
 * others (panel-verdicts), and sometimes only on the already-computed
 * plain-language object (`pl.boundaries`, e.g. the registry's own
 * uniqueBoundaries()). Collect and dedupe across all of them so the section
 * shows real limits whichever shape the artifact uses — never a fabricated
 * "nothing to show" line when the array is simply empty.
 */
function collectNotToDo(item: Json, pl: PlainLanguage): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const push = (raw: unknown) => {
    for (const value of arr(raw)) {
      const part = str(value).trim();
      if (part.length < 3) continue;
      const key = part.toLowerCase();
      if (seen.has(key)) continue;
      seen.add(key);
      out.push(part);
    }
  };

  push(item.boundaries);
  push(item.warnings);
  const notToDo = str(item.not_to_do);
  if (notToDo) push(notToDo.split(/[,;]/));
  if (isObject(item.decision_card)) push((item.decision_card as Json).boundary);
  push(pl.boundaries);

  return out;
}

function collectDocumentTargets(item: Json, pl: PlainLanguage): DocumentTarget[] {
  const seen = new Set<string>();
  const targets: DocumentTarget[] = [];

  const addPath = (label: string, value: unknown) => {
    const path = str(value).trim();
    if (!path || seen.has(`path:${path}`)) return;
    seen.add(`path:${path}`);
    targets.push({ key: `path:${path}`, label, kind: "path", value: path });
  };

  const addEvidence = (label: string, value: unknown) => {
    const ref = evidenceRef(value);
    if (!ref || seen.has(`evidence:${ref}`)) return;
    seen.add(`evidence:${ref}`);
    targets.push({ key: `evidence:${ref}`, label, kind: "evidence", value: ref });
  };

  addPath("candidate JSON", item.json_path);
  addPath("review JSON", item.json_path);
  addPath("review markdown", item.markdown_path);
  addPath("source path", item.source_path);
  addPath("target path", item.target_path);

  addEvidence("source evidence", item.source_trace_ref);
  if (isObject(item.console_source)) addEvidence("source evidence", item.console_source.evidence_ref);
  for (const ref of pl.evidence ?? []) addEvidence("evidence ref", ref);

  return targets;
}

function FieldRow({ label, value, mono = false }: { label: string; value: unknown; mono?: boolean }) {
  return (
    <div className="grid gap-1 rounded-lg bg-slate-50 px-3 py-2 sm:grid-cols-[150px_1fr]">
      <div className="section-label pt-0.5">{label}</div>
      <div
        className={`min-w-0 whitespace-pre-wrap break-words text-[12.5px] leading-relaxed text-ink-2 ${
          mono ? "mono" : ""
        }`}
      >
        {scalar(value)}
      </div>
    </div>
  );
}

function KeyValueGrid({ value }: { value: unknown }) {
  if (!isObject(value) || Object.keys(value).length === 0) {
    return <p className="text-[12.5px] text-ink-3">No structured fields surfaced.</p>;
  }
  return (
    <div className="grid gap-2">
      {Object.entries(value).map(([key, entry]) => (
        <FieldRow key={key} label={key} value={entry} mono={typeof entry !== "string"} />
      ))}
    </div>
  );
}

function FindingList({ findings }: { findings: unknown }) {
  const rows = arr(findings).filter(isObject);
  if (!rows.length) return null;
  return (
    <div className="mt-2 flex flex-col gap-2">
      {rows.map((finding, index) => (
        <div key={index} className="rounded-lg border border-line bg-white px-3 py-2">
          <div className="flex flex-wrap items-center gap-1.5">
            {str(finding.severity) ? <Chip tone="orange">{str(finding.severity)}</Chip> : null}
            {str(finding.status) ? <Chip tone="gray">{str(finding.status)}</Chip> : null}
          </div>
          <p className="mt-1 whitespace-pre-wrap text-[12.5px] leading-relaxed text-ink-2">
            {str(finding.claim) || str(finding.rationale) || scalar(finding)}
          </p>
          {arr(finding.evidence_refs).length ? (
            <div className="mt-2">
              <div className="section-label mb-1">Evidence</div>
              <div className="flex flex-col gap-1">
                {arr(finding.evidence_refs).map((ref, refIndex) => (
                  <code key={refIndex} className="mono break-all text-[11px] text-ink-3">
                    {String(ref)}
                  </code>
                ))}
              </div>
            </div>
          ) : null}
        </div>
      ))}
    </div>
  );
}

function LensFindings({ value }: { value: unknown }) {
  const lenses = arr(value).filter(isObject);
  if (!lenses.length) return null;
  return (
    <div className="flex flex-col gap-3">
      {lenses.map((lens, index) => (
        <div key={index} className="rounded-xl border border-line bg-slate-50 p-3">
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div>
              <div className="text-[13px] font-semibold text-ink">
                {str(lens.lens_ref) || str(lens.name) || `Lens ${index + 1}`}
              </div>
              {str(lens.route_evidence_ref) ? (
                <code className="mono mt-0.5 block break-all text-[11px] text-ink-3">
                  {str(lens.route_evidence_ref)}
                </code>
              ) : null}
            </div>
            <div className="flex flex-wrap gap-1">
              {str(lens.status) ? <Chip tone={str(lens.status) === "failed" ? "red" : "blue"}>{str(lens.status)}</Chip> : null}
              {lens.confidence != null ? <Chip tone="orange">conf {String(lens.confidence)}</Chip> : null}
            </div>
          </div>
          <FindingList findings={lens.findings} />
        </div>
      ))}
    </div>
  );
}

function AnnotationList({ annotations }: { annotations: unknown }) {
  const rows = arr(annotations).filter(isObject);
  if (!rows.length) return null;
  return (
    <div className="flex flex-col gap-3">
      {rows.map((annotation, index) => (
        <div key={index} className="rounded-xl border border-line bg-white p-3">
          <div className="flex flex-wrap items-center gap-1.5">
            {str(annotation.kind) ? <Chip tone="violet">{str(annotation.kind)}</Chip> : null}
            {str(annotation.decision) ? <Chip tone="blue">{str(annotation.decision)}</Chip> : null}
            {str(annotation.original_decision) ? (
              <Chip tone="orange">original {str(annotation.original_decision)}</Chip>
            ) : null}
            {annotation.confidence != null ? <Chip tone="orange">conf {String(annotation.confidence)}</Chip> : null}
          </div>
          {str(annotation.rationale) ? (
            <p className="mt-2 whitespace-pre-wrap text-[12.5px] leading-relaxed text-ink-2">
              {str(annotation.rationale)}
            </p>
          ) : null}
          <div className="mt-3">
            <LensFindings value={annotation.lens_findings} />
          </div>
        </div>
      ))}
    </div>
  );
}

export function DecisionDetailsDialog({
  area,
  itemId,
  item,
  pl,
  formSpec,
  buttonLabel = "Details",
}: {
  area: string;
  itemId: string;
  item: Json;
  pl: PlainLanguage;
  formSpec: DecisionFormSpec;
  buttonLabel?: string;
}) {
  const [open, setOpen] = useState(false);
  const [tab, setTab] = useState<DetailTab>("claim");
  const [activeTarget, setActiveTarget] = useState<DocumentTarget | null>(null);
  const [loadedDocument, setLoadedDocument] = useState<LoadedDocument | null>(null);
  const [loadingDocument, setLoadingDocument] = useState(false);
  const [idCopied, setIdCopied] = useState(false);
  const targets = useMemo(() => collectDocumentTargets(item, pl), [item, pl]);
  const notToDo = useMemo(() => collectNotToDo(item, pl), [item, pl]);

  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    window.addEventListener("keydown", onKey);
    return () => {
      document.body.style.overflow = previousOverflow;
      window.removeEventListener("keydown", onKey);
    };
  }, [open]);

  async function loadDocument(target: DocumentTarget) {
    setTab("source");
    setActiveTarget(target);
    setLoadingDocument(true);
    setLoadedDocument(null);
    const query =
      target.kind === "evidence"
        ? `evidence_ref=${encodeURIComponent(target.value)}`
        : `path=${encodeURIComponent(target.value)}`;
    try {
      const response = await fetch(`/api/document?${query}`);
      const body = (await response.json()) as LoadedDocument;
      setLoadedDocument(body);
    } catch (error) {
      setLoadedDocument({
        ok: false,
        stderr: error instanceof Error ? error.message : "failed to load document",
      });
    } finally {
      setLoadingDocument(false);
    }
  }

  async function copyItemId() {
    try {
      await navigator.clipboard.writeText(itemId);
      setIdCopied(true);
      window.setTimeout(() => setIdCopied(false), 1200);
    } catch {
      /* clipboard unavailable */
    }
  }

  const tabs: { key: DetailTab; label: string }[] = [
    { key: "claim", label: "Claim" },
    { key: "logic", label: "Logic" },
    { key: "source", label: "Source" },
    { key: "raw", label: "Raw" },
  ];

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="rounded-lg border border-line bg-white px-2.5 py-1.5 text-[11.5px] font-semibold text-ink-2 transition hover:border-slate-300 hover:bg-slate-50 active:translate-y-px"
      >
        {buttonLabel}
      </button>

      {open ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-950/35 px-4 py-6 backdrop-blur-sm">
          <div className="flex max-h-[92dvh] w-full max-w-5xl flex-col overflow-hidden rounded-2xl border border-line bg-white shadow-2xl">
            <div className="flex items-start justify-between gap-4 border-b border-line px-5 py-4">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-1.5">
                <Chip tone="violet">{area}</Chip>
                <button
                  type="button"
                  onClick={copyItemId}
                  title={itemId}
                  className="rounded-md border border-line px-1.5 py-0.5 text-[10.5px] font-semibold text-ink-3 hover:bg-slate-50"
                >
                  {idCopied ? "copied" : "copy id"}
                </button>
              </div>
                <h2 className="mt-2 max-w-3xl text-[18px] font-semibold leading-tight text-ink">
                  {pl.headline || itemId}
                </h2>
              </div>
              <button
                type="button"
                onClick={() => setOpen(false)}
                className="rounded-lg border border-line px-2.5 py-1.5 text-[12px] font-semibold text-ink-2 hover:bg-slate-50 active:translate-y-px"
              >
                Close
              </button>
            </div>

            <div className="flex flex-wrap gap-1 border-b border-line bg-slate-50 px-5 py-2">
              {tabs.map((candidate) => (
                <button
                  key={candidate.key}
                  type="button"
                  onClick={() => setTab(candidate.key)}
                  className={`rounded-lg px-3 py-1.5 text-[12px] font-semibold transition ${
                    tab === candidate.key ? "bg-white text-ink shadow-sm" : "text-ink-3 hover:text-ink"
                  }`}
                >
                  {candidate.label}
                </button>
              ))}
            </div>

            <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
              {tab === "claim" ? (
                <div className="grid gap-3">
                  {notToDo.length ? (
                    <div className="rounded-xl border border-amber-200 bg-amber-50 p-3">
                      <div className="section-label mb-2 text-amber-700">What this will NOT do</div>
                      <ul className="flex flex-col gap-1.5">
                        {notToDo.map((boundary) => (
                          <li key={boundary} className="flex items-start gap-2 text-[12.5px] leading-relaxed text-amber-900">
                            <span className="mt-[7px] h-1 w-1 shrink-0 rounded-full bg-amber-500" />
                            {boundary}
                          </li>
                        ))}
                      </ul>
                    </div>
                  ) : null}
                  <div className="rounded-xl border border-line bg-white p-3">
                    <div className="section-label mb-1">Full Claim</div>
                    <p className="whitespace-pre-wrap text-[13px] leading-relaxed text-ink">
                      {str(item.object) || pl.body || "No claim body surfaced."}
                    </p>
                  </div>
                  <div className="grid gap-2 md:grid-cols-2">
                    <FieldRow label="Entity" value={item.entity ?? pl.headline} mono />
                    <FieldRow label="Predicate" value={item.predicate} mono />
                    <FieldRow label="State" value={item.state ?? item.status} />
                    <FieldRow label="Visibility" value={item.visibility} />
                    <FieldRow label="Confidence" value={item.confidence} />
                    <FieldRow label="Derivation" value={item.derivation} />
                    <FieldRow label="Claim type" value={item.claim_type} />
                    <FieldRow label="Promotion basis" value={item.promotion_basis} mono />
                    <FieldRow label="Created" value={item.created_at} />
                    <FieldRow label="Observed" value={item.observation_date ?? item.observed_at} />
                  </div>
                </div>
              ) : null}

              {tab === "logic" ? (
                <div className="grid gap-4">
                  <div className="rounded-xl border border-line bg-white p-3">
                    <div className="section-label mb-2">Decision Meanings</div>
                    <div className="grid gap-2 sm:grid-cols-2">
                      {formSpec.options.map((option) => (
                        <div key={option.value} className="rounded-lg bg-slate-50 px-3 py-2">
                          <div className="text-[12.5px] font-semibold text-ink">{option.label}</div>
                          <p className="mt-0.5 text-[12px] leading-relaxed text-ink-3">{option.description}</p>
                        </div>
                      ))}
                    </div>
                  </div>

                  {area === "memory-adjudicate" ? (
                    <div className="rounded-xl border border-line bg-white p-3">
                      <div className="section-label mb-2">Promotion Basis Allowlist</div>
                      <div className="grid gap-2">
                        {PROMOTION_BASES.map(([basis, meaning]) => (
                          <div key={basis} className="grid gap-1 rounded-lg bg-slate-50 px-3 py-2 sm:grid-cols-[210px_1fr]">
                            <code className="mono text-[11.5px] text-ink">{basis}</code>
                            <span className="text-[12.5px] leading-relaxed text-ink-2">{meaning}</span>
                          </div>
                        ))}
                      </div>
                    </div>
                  ) : null}

                  {str(item.next_action) || str(item.reason) || str(item.rationale) ? (
                    <div className="rounded-xl border border-line bg-white p-3">
                      <div className="section-label mb-2">Current Reasoning</div>
                      <div className="grid gap-2">
                        <FieldRow label="Next action" value={item.next_action} />
                        <FieldRow label="Reason" value={item.reason} />
                        <FieldRow label="Rationale" value={item.rationale} />
                      </div>
                    </div>
                  ) : null}

                  {arr(item.annotations).length ? (
                    <div>
                      <div className="section-label mb-2">Panel Annotations</div>
                      <AnnotationList annotations={item.annotations} />
                    </div>
                  ) : null}

                  {arr(item.lens_findings).length ? (
                    <div>
                      <div className="section-label mb-2">Lens Findings</div>
                      <LensFindings value={item.lens_findings} />
                    </div>
                  ) : null}
                </div>
              ) : null}

              {tab === "source" ? (
                <div className="grid gap-4">
                  <div className="rounded-xl border border-line bg-white p-3">
                    <div className="section-label mb-2">Source Refs</div>
                    <div className="flex flex-col gap-1.5">
                      {[str(item.source_trace_ref), ...(pl.evidence ?? [])].filter(Boolean).map((ref, index) => (
                        <code key={`${ref}-${index}`} className="mono break-all text-[11.5px] text-ink-2">
                          {ref}
                        </code>
                      ))}
                    </div>
                  </div>

                  <div className="rounded-xl border border-line bg-white p-3">
                    <div className="section-label mb-2">Qualifiers</div>
                    <KeyValueGrid value={item.qualifiers} />
                  </div>

                  <div className="rounded-xl border border-line bg-white p-3">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div>
                        <div className="section-label">Document Viewer</div>
                        <p className="mt-1 text-[12px] text-ink-3">
                          Loads allowed Compound files or evidence excerpts on demand.
                        </p>
                      </div>
                      <div className="flex flex-wrap gap-1.5">
                        {targets.length ? (
                          targets.map((target) => (
                            <button
                              key={target.key}
                              type="button"
                              onClick={() => loadDocument(target)}
                              className="rounded-lg border border-line bg-white px-2.5 py-1.5 text-[11.5px] font-semibold text-ink-2 hover:bg-slate-50 active:translate-y-px"
                              title={target.value}
                            >
                              {target.label}: {compactLabel(target.value)}
                            </button>
                          ))
                        ) : (
                          <span className="text-[12px] text-ink-3">No document ref surfaced.</span>
                        )}
                      </div>
                    </div>

                    <div className="mt-3 rounded-xl border border-line bg-slate-950 p-3 text-slate-100">
                      {loadingDocument ? (
                        <p className="text-[12.5px] text-slate-300">Loading document…</p>
                      ) : loadedDocument ? (
                        loadedDocument.ok ? (
                          <div>
                            <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                              <code className="mono break-all text-[11px] text-slate-300">
                                {loadedDocument.title || activeTarget?.value}
                              </code>
                              {loadedDocument.truncated ? <Chip tone="orange">truncated at 1 MB</Chip> : null}
                            </div>
                            <pre className="max-h-[46dvh] overflow-auto whitespace-pre-wrap break-words text-[11.5px] leading-relaxed">
                              {loadedDocument.content}
                            </pre>
                          </div>
                        ) : (
                          <p className="text-[12.5px] text-red-200">
                            {loadedDocument.stderr || "Document failed to load."}
                          </p>
                        )
                      ) : (
                        <p className="text-[12.5px] text-slate-400">Pick a source above to load the full available document.</p>
                      )}
                    </div>
                  </div>
                </div>
              ) : null}

              {tab === "raw" ? (
                <pre className="max-h-[62dvh] overflow-auto rounded-xl border border-line bg-slate-950 p-4 text-[11.5px] leading-relaxed text-slate-100">
                  {JSON.stringify(item, null, 2)}
                </pre>
              ) : null}
            </div>
          </div>
        </div>
      ) : null}
    </>
  );
}
