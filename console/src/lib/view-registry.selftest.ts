import { stubAiConsole } from "@/ai-console";

import { renderViewInvocation } from "./view-registry";

/**
 * The depth test for Phase 5: proves `renderViewInvocation` actually
 * validates, not just typechecks. `scripts/view-registry-smoke.mjs` runs this
 * under `bun` (the kit components it renders are `.tsx`, so plain `node`
 * can't execute this file — no JSX transform).
 */
export type SelfTestResult = { ok: boolean; details: string[] };

export async function runViewRegistrySelfTest(): Promise<SelfTestResult> {
  const details: string[] = [];
  let ok = true;

  // (a) the stub's own fixture proposal must resolve cleanly through the registry.
  const proposal = await stubAiConsole.proposeView("smoke", { route: "/decide" });
  if (proposal.components.length === 0) {
    ok = false;
    details.push("FAIL: stub fixture proposal carries zero components");
  }
  for (const inv of proposal.components) {
    const result = renderViewInvocation(inv);
    if (!result.ok) {
      ok = false;
      details.push(`FAIL fixture invocation "${inv.view}": expected ok:true, got error "${result.error}"`);
    } else {
      details.push(`ok: fixture invocation "${inv.view}" rendered`);
    }
  }

  // (b) deliberately malformed props must come back ok:false with an error —
  // and, just as important, must not throw.
  let malformed: ReturnType<typeof renderViewInvocation>;
  try {
    malformed = renderViewInvocation({ view: "stat-tile", props: { label: 42, value: {} } });
  } catch (err) {
    ok = false;
    details.push(`FAIL: renderViewInvocation threw on malformed props instead of returning ok:false (${err})`);
    malformed = { ok: false, error: "threw" };
  }
  if (malformed.ok) {
    ok = false;
    details.push("FAIL: malformed props (label: number, value: object) resolved ok:true");
  } else {
    details.push(`ok: malformed props rejected without throwing ("${malformed.error}")`);
  }

  // (c) an unknown view name must also come back ok:false, not throw.
  const unknown = renderViewInvocation({ view: "does-not-exist", props: {} });
  if (unknown.ok) {
    ok = false;
    details.push("FAIL: unknown view name resolved ok:true");
  } else {
    details.push(`ok: unknown view name rejected without throwing ("${unknown.error}")`);
  }

  return { ok, details };
}
