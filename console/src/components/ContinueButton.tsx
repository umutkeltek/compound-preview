"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";

import { CliCaption } from "@/components/CopyCmd";
import { shortBlockedRefs } from "@/lib/work-labels";

type DecisionRefs = {
  work_ids?: string[];
  receipt_ids?: string[];
  transition_ids?: string[];
  proof_refs?: string[];
};

type Result = {
  ok: boolean;
  stdout?: string;
  stderr?: string;
  refs?: DecisionRefs;
};

type Json = Record<string, unknown>;

type ContinueStep = {
  action?: string;
  status?: string;
  work_id?: string;
  no_action?: string;
  wait_reason?: string;
};

type ContinuePayload = {
  schema_version?: string;
  status?: string;
  action?: string;
  work_id?: string;
  step_count?: number;
  steps?: ContinueStep[];
  wait_reason?: string;
  blocked_on?: string[];
  next_action?: string;
  result_path?: string;
  no_action?: string;
};

export type ContinueRequest = {
  area: string;
  itemId: string;
  option: string;
  fields?: Record<string, string>;
  command: string[];
  note: string;
};

function firstJsonObject(text: string): string | null {
  const start = text.indexOf("{");
  if (start < 0) return null;
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = start; i < text.length; i++) {
    const ch = text[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (ch === "\\") escaped = true;
      else if (ch === "\"") inString = false;
      continue;
    }
    if (ch === "\"") {
      inString = true;
      continue;
    }
    if (ch === "{") depth++;
    if (ch === "}") {
      depth--;
      if (depth === 0) return text.slice(start, i + 1);
    }
  }
  return null;
}

function parseContinuePayload(result: Result): ContinuePayload | null {
  const json = firstJsonObject(result.stdout ?? "");
  if (!json) return null;
  try {
    const parsed = JSON.parse(json) as Json;
    const schema = typeof parsed.schema_version === "string" ? parsed.schema_version : "";
    if (!schema.startsWith("compound.work_continue")) return null;
    return parsed as ContinuePayload;
  } catch {
    return null;
  }
}

function labelForAction(action: string, status?: string): string {
  switch (action) {
    case "admit":
      return "Added to work";
    case "prepare":
      return "Prepared for executor";
    case "claim":
      return "Started";
    case "advance":
      return status === "running" ? "Moved to running" : "Continued";
    case "close":
      return "Finished with proof";
    case "none":
      return "No safe move";
    default:
      return action ? action.replace(/_/g, " ") : "Continued";
  }
}

function plainReason(reason?: string): string {
  if (!reason) return "";
  return reason.replace(/_/g, " ");
}

function relativeRecordPath(path?: string): string {
  if (!path) return "";
  const parts = path.split("/").filter(Boolean);
  if (parts.length <= 2) return path;
  return parts.slice(-2).join("/");
}

function ContinueSummary({ result }: { result: Result }) {
  const payload = parseContinuePayload(result);
  if (!payload) {
    return <>{result.ok ? "Continued." : result.stderr || result.stdout || "Command failed."}</>;
  }

  const steps = Array.isArray(payload.steps) ? payload.steps : payload.action ? [payload as ContinueStep] : [];
  const waitReason = plainReason(payload.wait_reason);
  const blockedRefs = shortBlockedRefs(payload.blocked_on);
  const recordPath = relativeRecordPath(payload.result_path);
  const headline = result.ok
    ? waitReason
      ? `Now waiting for ${waitReason}.`
      : steps.length
        ? `Moved ${steps.length} step${steps.length === 1 ? "" : "s"}.`
        : payload.no_action || "No safe move is ready."
    : payload.no_action || result.stderr || "Command failed.";

  return (
    <div className="flex flex-col gap-2">
      <div className="font-semibold">{headline}</div>

      {steps.length ? (
        <ol className="flex flex-col gap-1">
          {steps.map((step, index) => (
            <li key={`${step.action ?? "step"}-${index}`} className="flex gap-2">
              <span className="shrink-0 text-emerald-500">{index + 1}.</span>
              <span>
                {labelForAction(step.action ?? "", step.status)}
                {step.work_id ? <span className="text-ink-3"> · {step.work_id}</span> : null}
              </span>
            </li>
          ))}
        </ol>
      ) : null}

      {blockedRefs ? <div className="text-ink-3">Waiting on {blockedRefs}</div> : null}
      {payload.next_action ? <div className="text-ink-3">{payload.next_action}</div> : null}
      {recordPath ? <div className="text-ink-3">Record: {recordPath}</div> : null}
    </div>
  );
}

function CopyOnlyContinue({ request }: { request: ContinueRequest }) {
  return (
    <div className="flex flex-col gap-2 rounded-xl border border-hair bg-soft px-3 py-2">
      <div className="text-[12px] font-semibold text-muted">Copy command</div>
      <CliCaption argv={request.command[0] === "compound" ? request.command.slice(1) : request.command} />
      <p className="text-[11.5px] leading-relaxed text-muted">
        This console is read-only until it is launched with an explicit home via <code>compound console --operator</code>.
      </p>
    </div>
  );
}

function LiveContinueButton({ request }: { request: ContinueRequest }) {
  const router = useRouter();
  const [pending, setPending] = useState(false);
  const [result, setResult] = useState<Result | null>(null);

  async function run() {
    setPending(true);
    setResult(null);
    try {
      const response = await fetch("/api/decision/execute", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          area: request.area,
          itemId: request.itemId,
          option: request.option,
          fields: request.fields ?? {},
        }),
      });
      const text = await response.text();
      let body: Result;
      try {
        body = JSON.parse(text) as Result;
      } catch {
        body = { ok: false, stderr: text || `Request failed with ${response.status}` };
      }
      if (!response.ok && body.ok) {
        body = { ...body, ok: false, stderr: body.stderr || `Request failed with ${response.status}` };
      }
      setResult(body);
      if (body.ok) {
        window.setTimeout(() => router.refresh(), 250);
      }
    } catch (error) {
      setResult({ ok: false, stderr: error instanceof Error ? error.message : "Request failed." });
    } finally {
      setPending(false);
    }
  }

  return (
    <div className="flex flex-col gap-2 rounded-xl border border-emerald-100 bg-emerald-50 px-3 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          disabled={pending}
          onClick={run}
          className="rounded-lg bg-emerald-600 px-3 py-2 text-[12px] font-semibold text-white disabled:cursor-wait disabled:opacity-55"
        >
          {pending ? "Continuing..." : "Continue"}
        </button>
        <CliCaption argv={request.command[0] === "compound" ? request.command.slice(1) : request.command} />
      </div>
      <p className="text-[11.5px] leading-relaxed text-emerald-800">{request.note}</p>

      {result ? (
        <div
          className={`rounded-lg border px-3 py-2 text-[12px] leading-relaxed ${
            result.ok ? "border-emerald-200 bg-white text-emerald-700" : "border-red-200 bg-red-50 text-red-600"
          }`}
        >
          <ContinueSummary result={result} />
        </div>
      ) : null}
    </div>
  );
}

export function ContinueButton({
  request,
  mutationsReady,
}: {
  request: ContinueRequest | null;
  mutationsReady: boolean;
}) {
  if (!request) return null;
  return mutationsReady ? <LiveContinueButton request={request} /> : <CopyOnlyContinue request={request} />;
}
