"use client";

import * as Dialog from "@radix-ui/react-dialog";
import { useRouter } from "next/navigation";
import { useState } from "react";

import { Button, ConfirmDialog } from "@/components/kit";
import { CliCaption, CopyCmd } from "@/components/CopyCmd";
import type { ActionOption, WorkActionRowData } from "@/lib/work-pipeline";
import { WORK_COPY } from "@/lib/work-copy";

/*
 * The contextual governed state-transition row (the buried-<details> fix).
 * Options are the EXISTING work-advance state machine (WORK_TRANSITIONS /
 * workOptionsFor), resolved server-side and handed down; this component only
 * renders them and posts the human click to /api/decision/execute (area
 * work-advance) — the one governed mutation endpoint. Non-terminal moves
 * confirm through the kit ConfirmDialog; terminal closes (complete/fail/cancel)
 * open a close dialog carrying the required summary + move-type, because the
 * CLI requires them. Optimistic states reuse the kit `.action-*` motion.
 */

export type { ActionOption, WorkActionRowData };

type Phase = "idle" | "pending" | "error";

const PRIMARY_ORDER = ["running", "verifying", "claimed", "ready", "admitted", "completed", "held", "blocked"];
const MOVE_TYPES = [
  { value: "control_plane_repair", label: "Control-plane repair" },
  { value: "outward_delta", label: "Outward delta" },
  { value: "memory_delta", label: "Memory delta" },
  { value: "rule_delta", label: "Rule delta" },
  { value: "audit_only", label: "Audit only" },
];

function primaryValue(options: ActionOption[]): string {
  for (const value of PRIMARY_ORDER) {
    if (options.some((o) => o.value === value)) return value;
  }
  return options[0]?.value ?? "";
}

const INPUT =
  "w-full rounded-md border border-hair bg-panel px-3 py-2 text-sm text-ink outline-none " +
  "focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus";

function LiveWorkActionRow({
  data,
  onDone,
  dense = false,
}: {
  data: WorkActionRowData;
  onDone?: () => void;
  dense?: boolean;
}) {
  const router = useRouter();
  const [active, setActive] = useState<ActionOption | null>(null);
  const [phase, setPhase] = useState<Phase>("idle");
  const [error, setError] = useState<string | null>(null);
  const [summary, setSummary] = useState("");
  const [moveType, setMoveType] = useState(MOVE_TYPES[0].value);

  if (!data.hasTransitions || data.options.length === 0) {
    return (
      <p className="text-xs text-muted">
        No governed move is available from <span className="mono text-2xs">{data.state || "this state"}</span> —
        this work is terminal or waiting on an executor.
      </p>
    );
  }

  const primary = primaryValue(data.options);

  function openOption(option: ActionOption) {
    setError(null);
    setSummary("");
    setMoveType(MOVE_TYPES[0].value);
    setActive(option);
  }

  async function submit() {
    if (!active) return;
    if (active.needsSummary && !summary.trim()) {
      setError(`${active.label} needs a close summary.`);
      return;
    }
    setPhase("pending");
    setError(null);
    const fields: Record<string, string> = {};
    if (active.needsSummary) fields.summary = summary.trim();
    if (active.needsMoveType) fields.move_type = moveType;

    try {
      const response = await fetch("/api/decision/execute", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ area: data.area, itemId: data.itemId, option: active.value, fields, confirmed: true }),
      });
      const body = (await response.json().catch(() => null)) as { ok?: boolean; stderr?: string; stdout?: string } | null;
      if (body?.ok) {
        setPhase("idle");
        setActive(null);
        router.refresh();
        onDone?.();
      } else {
        setPhase("error");
        setError(body?.stderr || body?.stdout || `Request failed with ${response.status}.`);
      }
    } catch (err) {
      setPhase("error");
      setError(err instanceof Error ? err.message : "Request failed.");
    }
  }

  return (
    <>
      <div className={`flex flex-wrap items-center gap-2 ${dense ? "" : "justify-end"}`}>
        {data.options.map((option) => {
          const variant =
            option.value === primary ? "primary" : option.terminal && option.value !== "completed" ? "danger" : "secondary";
          return (
            <Button key={option.value} variant={variant} onClick={() => openOption(option)}>
              {option.label}
            </Button>
          );
        })}
      </div>

      {/* Non-terminal moves: kit ConfirmDialog (title, body, argv preview). */}
      {active && !active.needsSummary ? (
        <ConfirmDialog
          open
          onOpenChange={(open) => !open && setActive(null)}
          title={active.label}
          body={active.description}
          policyNote="Every mutation is gated by a PolicyDecision at the adapter. This records a human click."
          argv={active.argv}
          pending={phase === "pending"}
          confirmLabel={active.label}
          onConfirm={submit}
        />
      ) : null}

      {/* Terminal closes: dialog carrying the required summary (+ move type). */}
      {active && active.needsSummary ? (
        <Dialog.Root open onOpenChange={(open) => !open && setActive(null)}>
          <Dialog.Portal>
            <Dialog.Overlay className="fixed inset-0 z-50 bg-strong/35 backdrop-blur-sm motion-reduce:backdrop-blur-none" />
            <Dialog.Content className="fixed top-1/2 left-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-lg border border-hair bg-panel p-5 shadow-lg focus:outline-none">
              <Dialog.Title className="text-md font-semibold text-ink">{active.label}</Dialog.Title>
              <Dialog.Description className="mt-2 text-sm leading-relaxed text-muted">{active.description}</Dialog.Description>

              <label className="mt-3 flex flex-col gap-1">
                <span className="text-2xs font-bold uppercase tracking-wide text-faint">
                  Close summary<span className="text-crit"> *</span>
                </span>
                <textarea
                  value={summary}
                  onChange={(event) => setSummary(event.target.value)}
                  placeholder="What was done / why it is closing."
                  className={`min-h-20 resize-y ${INPUT}`}
                />
              </label>

              {active.needsMoveType ? (
                <label className="mt-3 flex flex-col gap-1">
                  <span className="text-2xs font-bold uppercase tracking-wide text-faint">Move type</span>
                  <select value={moveType} onChange={(event) => setMoveType(event.target.value)} className={INPUT}>
                    {MOVE_TYPES.map((type) => (
                      <option key={type.value} value={type.value}>
                        {type.label}
                      </option>
                    ))}
                  </select>
                </label>
              ) : null}

              {active.argv ? (
                <pre className="mono mt-3 overflow-x-auto rounded-md border border-hair bg-soft px-3 py-2 text-2xs leading-relaxed text-muted">
                  compound {active.argv.join(" ")}
                </pre>
              ) : null}
              {error ? <p className="mt-2 text-xs leading-snug text-crit">{error}</p> : null}

              <div className="mt-4 flex justify-end gap-2">
                <Dialog.Close asChild>
                  <Button variant="secondary">Cancel</Button>
                </Dialog.Close>
                <Button variant={active.value === "completed" ? "primary" : "danger"} disabled={phase === "pending"} onClick={submit}>
                  {phase === "pending" ? "Working…" : active.label}
                </Button>
              </div>
            </Dialog.Content>
          </Dialog.Portal>
        </Dialog.Root>
      ) : null}

      {error && !active ? <p className="mt-1 text-xs text-crit">{error}</p> : null}
    </>
  );
}

function CopyOnlyWorkActions({ data, dense }: { data: WorkActionRowData; dense: boolean }) {
  return (
    <div className={`flex flex-col gap-2 ${dense ? "" : "items-end"}`}>
      {data.options.map((option) => (
        <div key={option.value} className="flex max-w-full flex-wrap items-center gap-2 rounded-md border border-hair bg-soft px-3 py-2">
          <span className="text-xs font-semibold text-muted">Copy {option.label} command</span>
          <CliCaption argv={option.argv} />
        </div>
      ))}
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
        <p className="text-xs text-muted">{WORK_COPY.consoleReadOnly.prose}</p>
        <CopyCmd cmd={WORK_COPY.consoleReadOnly.command} />
      </div>
    </div>
  );
}

export function WorkActionRow({
  data,
  mutationsReady,
  onDone,
  dense = false,
}: {
  data: WorkActionRowData;
  mutationsReady: boolean;
  onDone?: () => void;
  dense?: boolean;
}) {
  if (!data.hasTransitions || data.options.length === 0) {
    return (
      <p className="text-xs text-muted">
        No governed move is available from <span className="mono text-2xs">{data.state || "this state"}</span> —
        this work is terminal or waiting on an executor.
      </p>
    );
  }
  if (!mutationsReady) return <CopyOnlyWorkActions data={data} dense={dense} />;
  return <LiveWorkActionRow data={data} onDone={onDone} dense={dense} />;
}
