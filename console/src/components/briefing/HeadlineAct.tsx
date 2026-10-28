"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";

import { CliCaption } from "@/components/CopyCmd";
import { Button } from "@/components/kit/Button";
import { ConfirmDialog } from "@/components/kit/ConfirmDialog";
import { authoritySummarySegments } from "@/lib/action-authority";
import type { HeadlineActDescriptor } from "@/lib/briefing";
import { buildHeadlineActionRequest } from "@/lib/governed-action-client";

type ExecuteResult = { ok: boolean; stdout?: string; stderr?: string };
type Phase = "idle" | "pending" | "confirmed" | "error";

/**
 * THE headline's one-click governed act (redesign plan Part 3). Routes an
 * AttentionGroup's action through the SAME `/api/decision/execute` endpoint the
 * Decide page uses — bespoke or generic pathway per the descriptor — so the
 * argv is re-resolved and gated server-side, never trusted from the client.
 *
 * Signature interaction (DESIGN.md §5): click → pending shimmer → confirm
 * settle with a ring-flash, or error → shake + inline message. When the act
 * needs confirmation (or a rationale the area requires), a ConfirmDialog shows
 * the resolved command first.
 */
export function HeadlineAct({
  act,
  mutationsEnabled,
}: {
  act: HeadlineActDescriptor;
  mutationsEnabled: boolean;
}) {
  const router = useRouter();
  const [phase, setPhase] = useState<Phase>("idle");
  const [message, setMessage] = useState<string | null>(null);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [rationale, setRationale] = useState("");

  const preview = act.argv[0] === "compound" ? act.argv.slice(1) : act.argv;
  const authority = act.authority;
  const confirmationBody = authority
    ? authoritySummarySegments(authority).join(" · ")
    : "Runs the current owner action through Compound; the adapter revalidates the target and policy before execution.";

  if (!mutationsEnabled) {
    // Honest fallback: no live button when mutations are gated off — the exact
    // command to run, and the seat in Decide where it's governed.
    return (
      <div className="flex flex-col items-start gap-1.5">
        <span className="rounded-md border border-hair bg-soft px-3 py-2 text-xs font-semibold text-muted">
          Terminal-only — enable the operator console to act
        </span>
        <CliCaption argv={preview} />
      </div>
    );
  }

  async function run(kind: "direct" | "confirm") {
    setPhase("pending");
    setMessage(null);
    const payload = buildHeadlineActionRequest(act, rationale, kind);
    try {
      const response = await fetch("/api/decision/execute", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(payload),
      });
      const body = (await response.json().catch(() => null)) as ExecuteResult | null;
      const result = body ?? { ok: false, stderr: `Request failed with ${response.status}` };
      if (result.ok) {
        setDialogOpen(false);
        setPhase("confirmed");
        router.refresh();
        window.setTimeout(() => setPhase("idle"), 900);
      } else {
        setPhase("error");
        setMessage(result.stderr || result.stdout || "Command failed.");
        window.setTimeout(() => setPhase("idle"), 1600);
      }
    } catch (error) {
      setPhase("error");
      setMessage(error instanceof Error ? error.message : "Request failed.");
      window.setTimeout(() => setPhase("idle"), 1600);
    }
  }

  function onPrimary() {
    if (act.requiresConfirmation || act.rationaleRequired) setDialogOpen(true);
    else void run("direct");
  }

  const stateClass =
    phase === "pending" ? "action-pending" : phase === "confirmed" ? "action-confirmed" : phase === "error" ? "action-error" : "";

  return (
    <div className="flex flex-col items-start gap-1.5">
      <Button variant="primary" tone="work" disabled={phase === "pending"} onClick={onPrimary} className={stateClass}>
        {phase === "pending" ? "Working…" : act.label}
      </Button>
      {phase === "error" && message ? <span className="text-xs leading-snug text-crit">{message}</span> : null}

      <ConfirmDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        title={act.label}
        body={confirmationBody}
        argv={preview}
        confirmLabel={act.label}
        pending={phase === "pending"}
        confirmDisabled={act.rationaleRequired && !rationale.trim()}
        onConfirm={() => void run("confirm")}
      >
        {act.rationaleRequired ? (
          <label className="flex flex-col gap-1">
            <span className="text-2xs font-bold uppercase tracking-wide text-faint">
              Rationale<span className="text-crit"> *</span>
            </span>
            <textarea
              value={rationale}
              onChange={(event) => setRationale(event.target.value)}
              placeholder="Why record this? It becomes provenance on the review."
              className="min-h-20 resize-y rounded-md border border-hair bg-panel px-3 py-2 text-sm text-ink outline-none focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
            />
          </label>
        ) : null}
      </ConfirmDialog>
    </div>
  );
}
