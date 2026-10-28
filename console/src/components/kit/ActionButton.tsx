"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import type { ReactNode } from "react";

import { Button, type ButtonVariant } from "./Button";

type ExecuteResult = { ok: boolean; stdout?: string; stderr?: string };
type ActionButtonState = "idle" | "pending" | "confirmed" | "error";

/** Same payload contract DecisionForm/PanelApplyButton post to /api/decision/execute today. */
export type ActionButtonPayload = {
  area: string;
  itemId: string;
  option: string;
  fields?: Record<string, string>;
};

/**
 * Governed submit wrapper (DESIGN.md §5's signature interaction): click ->
 * pending shimmer -> server confirm -> settle with a ring-flash + refresh, or
 * error -> shake + inline message + revert. `.action-pending` /
 * `.action-confirmed` / `.action-error` (globals.css) already turn themselves
 * off under prefers-reduced-motion, so this component needs no extra motion
 * branching.
 */
export function ActionButton({
  payload,
  variant = "primary",
  children,
  onSettled,
}: {
  payload: ActionButtonPayload;
  variant?: ButtonVariant;
  children: ReactNode;
  onSettled?: (result: ExecuteResult) => void;
}) {
  const router = useRouter();
  const [state, setState] = useState<ActionButtonState>("idle");
  const [message, setMessage] = useState<string | null>(null);

  async function run() {
    setState("pending");
    setMessage(null);
    try {
      const response = await fetch("/api/decision/execute", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(payload),
      });
      const body = (await response.json().catch(() => null)) as ExecuteResult | null;
      const result: ExecuteResult = body ?? { ok: false, stderr: `Request failed with ${response.status}` };
      onSettled?.(result);
      if (result.ok) {
        setState("confirmed");
        router.refresh();
        window.setTimeout(() => setState("idle"), 900);
      } else {
        setState("error");
        setMessage(result.stderr || result.stdout || "Command failed.");
        window.setTimeout(() => setState("idle"), 1400);
      }
    } catch (error) {
      setState("error");
      setMessage(error instanceof Error ? error.message : "Request failed.");
      window.setTimeout(() => setState("idle"), 1400);
    }
  }

  const stateClass =
    state === "pending"
      ? "action-pending"
      : state === "confirmed"
        ? "action-confirmed"
        : state === "error"
          ? "action-error"
          : "";

  return (
    <div className="flex flex-col items-start gap-1.5">
      <Button variant={variant} disabled={state === "pending"} onClick={run} className={stateClass}>
        {state === "pending" ? "Working…" : children}
      </Button>
      {state === "error" && message ? <span className="text-xs leading-snug text-crit">{message}</span> : null}
    </div>
  );
}
