import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { WorkActionRow } from "./WorkActionRow";
import type { WorkActionRowData } from "@/lib/work-pipeline";

export function runWorkActionRowSelfTest(): { ok: boolean; details: string[] } {
  const data: WorkActionRowData = {
    area: "work-advance",
    itemId: "work_fixture",
    state: "ready",
    hasTransitions: true,
    options: [
      {
        value: "claimed",
        label: "Start",
        tone: "primary",
        description: "Claim this work for an executor.",
        terminal: false,
        needsSummary: false,
        needsMoveType: false,
        argv: ["work", "advance", "work_fixture", "--to", "claimed", "--by", "operator-console"],
      },
    ],
  };
  const html = renderToStaticMarkup(
    createElement(WorkActionRow, { data, mutationsReady: false }),
  );
  const ok =
    !html.includes(">Start<") &&
    html.includes("Copy Start command") &&
    html.includes("compound work advance work_fixture --to claimed") &&
    html.includes("read-only");
  return { ok, details: [`${ok ? "ok" : "FAIL"} unpinned-work-copy-only: ${html}`] };
}
