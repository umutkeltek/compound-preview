import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { ContinueButton, type ContinueRequest } from "./ContinueButton";

type Check = { name: string; ok: boolean; detail: string };

const request: ContinueRequest = {
  area: "work-continue",
  itemId: "work_fixture",
  option: "continue",
  fields: { actor: "codex" },
  command: [
    "compound",
    "work",
    "continue",
    "--actor",
    "codex",
    "--expect-work",
    "work_fixture",
    "--until-wait",
    "--json",
  ],
  note: "Moves safe lifecycle steps until the next real wait.",
};

export function runContinueButtonSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  const copyOnly = renderToStaticMarkup(
    createElement(ContinueButton, { request, mutationsReady: false }),
  );
  push(
    "unpinned-copy-only",
    !copyOnly.includes(">Continue<") && copyOnly.includes("compound work continue") && copyOnly.includes("Copy command"),
    copyOnly,
  );

  return {
    ok: checks.every((check) => check.ok),
    details: checks.map((check) => `${check.ok ? "ok" : "FAIL"} ${check.name}: ${check.detail}`),
  };
}
