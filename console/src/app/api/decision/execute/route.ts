import { consoleMutationBlockedResponse } from "@/lib/console-mutations";
import { executeDecision } from "@/lib/decision-execute";
import { instanceUnpinnedBlockedResponse } from "@/lib/instance-pin";

/*
 * The single generic mutation endpoint. Replaces /api/reviews/record and
 * /api/work/act. Flow: look up the registry entry, validate id, re-resolve
 * item projection server-side, compute allowed options for this item, validate
 * the chosen option and fields, build argv, enforce the verb-prefix allowlist,
 * clamp confidence, then run compound. DEC-0021: every action is a human click.
 */
export async function POST(request: Request) {
  const blocked = consoleMutationBlockedResponse(request);
  if (blocked) return blocked;

  // Origin-pin: an instance started without an explicit home pin would silently
  // write the canonical home (the 2026-07-11 cross-port breach). Refuse the
  // mutation unless this server is pinned to a home the operator chose. See
  // instance-pin.ts.
  const unpinned = instanceUnpinnedBlockedResponse();
  if (unpinned) return unpinned;

  let body: Record<string, unknown>;
  try {
    body = (await request.json()) as Record<string, unknown>;
  } catch {
    return Response.json({ ok: false, stderr: "invalid JSON body" }, { status: 400 });
  }

  return executeDecision(body);
}
