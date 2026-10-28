import { NextResponse } from "next/server";

import { compoundFailure, runCompound, str } from "@/lib/compound";
import { findWorkbenchItem } from "@/lib/work-data";
import { buildActionRow } from "@/lib/work-pipeline";
import { mapEnvelopeSpine, parseWorkShow } from "@/lib/work-trace";

/*
 * Read-only projection behind the Trace drawer. GET only — no mutation ever
 * runs here (the drawer's action row posts to /api/decision/execute, the one
 * governed mutation endpoint). Built fresh from `compound work show <id>
 * --json` (envelope spine) + the workbench item (receipt_id, which work show
 * does not carry). The contextual state-transition options come from the
 * EXISTING work-advance state machine (WORK_TRANSITIONS / workOptionsFor) so
 * the drawer and the CLI can never disagree about what moves are legal.
 */

export const dynamic = "force-dynamic";

const WORK_ID = /^work_[a-z0-9]+$/;

export async function GET(request: Request) {
  const id = new URL(request.url).searchParams.get("id")?.trim() ?? "";
  if (!WORK_ID.test(id)) {
    return NextResponse.json({ error: "invalid work id" }, { status: 400 });
  }

  let raw: string;
  try {
    const result = await runCompound(["work", "show", id, "--json"]);
    raw = result.stdout;
  } catch (error) {
    const failure = compoundFailure(error);
    return NextResponse.json(
      { error: failure.stderr || failure.message || `work ${id} not found` },
      { status: 404 },
    );
  }

  const { projection, lifecycle } = parseWorkShow(raw);
  if (!projection) {
    return NextResponse.json({ error: `could not parse projection for ${id}` }, { status: 500 });
  }

  const workbenchItem = await findWorkbenchItem(id);
  const receiptId = str(workbenchItem?.receipt_id);

  const trace = mapEnvelopeSpine(projection, lifecycle, { receiptId });

  // Contextual governed transitions for the current state — the buried-<details>
  // fix. The mutation is still a human click through /api/decision/execute.
  return NextResponse.json({
    generatedAt: new Date().toISOString(),
    trace,
    actionRow: buildActionRow(id, trace.state),
  });
}
