import { NextResponse } from "next/server";

import { isLiveLens } from "@/lib/map/lens";
import { buildRegistryLens } from "@/lib/map/lenses/registry";
import { buildRuntimeLens } from "@/lib/map/lenses/runtime";

/*
 * Read-only lens projections for the System Map. GET only — no mutation.
 * The client hook polls these every 15s; each lens is built fresh from live
 * `compound … --json` / ~/.compound reads.
 */

export const dynamic = "force-dynamic";

export async function GET(
  request: Request,
  ctx: { params: Promise<{ lens: string }> },
) {
  const { lens } = await ctx.params;
  if (!isLiveLens(lens)) {
    return NextResponse.json(
      { error: `lens "${lens}" is not live yet` },
      { status: 404 },
    );
  }

  try {
    if (lens === "runtime") {
      return NextResponse.json(await buildRuntimeLens());
    }
    const url = new URL(request.url);
    const group = url.searchParams.get("group") === "none" ? "none" : "kind";
    return NextResponse.json(await buildRegistryLens({ group }));
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : "lens build failed" },
      { status: 500 },
    );
  }
}
