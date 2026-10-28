"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";

import { CliCaption } from "@/components/CopyCmd";

type Result = { ok: boolean; stdout?: string; stderr?: string };

/**
 * The one real verb button for a panel-verdicts group's safe batch act
 * (area "panel-apply" in the decision registry). A single primary button,
 * not the full DecisionForm option grid, since there is exactly one decision
 * here ("apply this run") and the spec calls for a plain labeled button.
 * Same pending/success/error idiom as DecisionForm/ContinueButton: disabled +
 * label swap while pending, a tick on success, inline error on failure.
 */
export function PanelApplyButton({
  itemId,
  area = "panel-apply",
  option = "apply",
  fields = {},
  label,
  argv,
}: {
  itemId: string;
  area?: string;
  option?: string;
  fields?: Record<string, string>;
  label: string;
  argv: string[];
}) {
  const router = useRouter();
  const [pending, setPending] = useState(false);
  const [done, setDone] = useState(false);
  const [result, setResult] = useState<Result | null>(null);

  async function run() {
    setPending(true);
    setResult(null);
    try {
      const response = await fetch("/api/decision/execute", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ area, itemId, option, fields }),
      });
      const body = (await response.json().catch(() => null)) as Result | null;
      const outcome: Result = body ?? { ok: false, stderr: `Request failed with ${response.status}` };
      setResult(outcome);
      if (outcome.ok) {
        setDone(true);
        router.refresh();
      }
    } catch (error) {
      setResult({ ok: false, stderr: error instanceof Error ? error.message : "Request failed." });
    } finally {
      setPending(false);
    }
  }

  return (
    <div className="flex flex-col items-start gap-1">
      <button
        type="button"
        onClick={run}
        disabled={pending || done}
        className="rounded-lg bg-ink px-3 py-1.5 text-[12px] font-semibold text-white disabled:cursor-wait disabled:opacity-55"
      >
        {pending ? "Working..." : done ? "Done" : label}
      </button>
      <CliCaption argv={argv} />
      {result && !result.ok ? (
        <span className="text-[11px] leading-relaxed text-red-600">
          {result.stderr || result.stdout || "Command failed."}
        </span>
      ) : null}
    </div>
  );
}
