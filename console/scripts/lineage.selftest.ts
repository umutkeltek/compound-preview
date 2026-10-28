/*
 * Falsifier for the public-alpha projection-only boundary. The former
 * private-fallback candidate reader behind /map/lineage is retired, so even a
 * valid candidate id and the legacy fallback environment must never cause a
 * direct read beneath inbox/memory-candidates. Traversal-shaped ids remain in
 * the fixture as adversarial controls for the older E17 failure mode.
 *
 * This test proves the FILESYSTEM-READ boundary, not a string predicate: it
 * plants real, parseable sentinel files outside the candidate directory
 * (inside COMPOUND_HOME and one level ABOVE it) plus a decoy with a
 * non-canonical name inside it, then asserts no id surfaces private file
 * contents (found:false, no sentinel in the projection). Before the fallback
 * retirement, the canonical-id checks resolved the planted direct children.
 *
 * Run from console/: `bun scripts/lineage.selftest.ts`
 * (bun resolves the `@/lib` alias lineage.ts pulls in).
 */

import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import type { Lineage } from "../src/lib/map/lineage";

type Check = { name: string; ok: boolean; detail: string };

const HOME_SENTINEL = "SECRET-HOME-SENTINEL";
const PARENT_SENTINEL = "SECRET-PARENT-SENTINEL";
const DECOY_ENTITY = "DECOY-ENTITY";
const INVALID_LENGTH_ENTITY = "INVALID-LENGTH-ENTITY";

export async function runLineageSelfTest(): Promise<{ ok: boolean; details: string[] }> {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  // Fixture world: COMPOUND_HOME = <tmp>/home, candidates dir inside it, and
  // sentinel JSON files OUTSIDE the candidates dir (one inside home, one a
  // level above home) that a traversal read would parse and surface.
  const tmpParent = mkdtempSync(path.join(tmpdir(), "lineage-selftest-"));
  const home = path.join(tmpParent, "home");
  const candidates = path.join(home, "inbox", "memory-candidates");
  try {
    mkdirSync(candidates, { recursive: true });
    writeFileSync(
      path.join(tmpParent, "secret-parent.json"),
      JSON.stringify({ entity: "leak", predicate: "leaked", object: PARENT_SENTINEL }),
    );
    writeFileSync(
      path.join(home, "secret.json"),
      JSON.stringify({ entity: "leak", predicate: "leaked", object: HOME_SENTINEL }),
    );
    writeFileSync(
      path.join(candidates, "cand_9df96833602e.json"),
      JSON.stringify({
        entity: "entity:canonical-12",
        predicate: "uses",
        object: "direct child read",
        created_at: "2026-07-23T00:00:00Z",
      }),
    );
    writeFileSync(
      path.join(candidates, "cand_0123456789abcdef.json"),
      JSON.stringify({ entity: "entity:canonical-16", predicate: "uses", object: "sixteen hex" }),
    );
    // Decoy: exists on disk, but its id is NOT the minted grammar. A read of
    // it proves the invalid-id path still touches the filesystem.
    writeFileSync(
      path.join(candidates, "cand_x.json"),
      JSON.stringify({ entity: DECOY_ENTITY, predicate: "should", object: "never surface" }),
    );
    for (const id of ["cand_0123456789abc", "cand_0123456789abcd", "cand_0123456789abcde"]) {
      writeFileSync(
        path.join(candidates, `${id}.json`),
        JSON.stringify({
          entity: INVALID_LENGTH_ENTITY,
          predicate: "should",
          object: "never surface",
        }),
      );
    }

    // Freeze the instance pin and gates BEFORE lineage.ts (and thus
    // compound.ts) is first imported: COMPOUND_HOME resolves once at module
    // load. COMPOUND_BIN points at a non-executable stub so the CLI
    // projection leg fails fast to null and the private fallback runs.
    writeFileSync(path.join(tmpParent, "failbin"), "");
    process.env.COMPOUND_HOME = home;
    process.env.COMPOUND_CONSOLE_PRIVATE_FALLBACKS = "1";
    process.env.COMPOUND_BIN = path.join(tmpParent, "failbin");

    const { traceLineage } = await import("../src/lib/map/lineage");

    const trace = async (id: string): Promise<{ lineage: Lineage | null; threw: string }> => {
      try {
        return { lineage: await traceLineage(id), threw: "" };
      } catch (err) {
        return { lineage: null, threw: String(err) };
      }
    };
    const leakFree = (lineage: Lineage | null, sentinel: string): boolean =>
      lineage !== null && !JSON.stringify(lineage).includes(sentinel);

    // 1. Decoded traversal that stays inside COMPOUND_HOME but escapes the
    //    candidate dir: home/secret.json is real and parseable, so found:true
    //    or a surfaced sentinel proves the read escaped.
    {
      const { lineage, threw } = await trace("cand_../../../../secret");
      push(
        "decoded-traversal-inside-home-blocked",
        !threw && lineage?.found === false && leakFree(lineage, HOME_SENTINEL),
        `found=${lineage?.found} threw=${threw || "no"}`,
      );
    }

    // 2. Decoded traversal that escapes COMPOUND_HOME entirely: the sentinel
    //    lives a level ABOVE home.
    {
      const { lineage, threw } = await trace("cand_../../../../../secret-parent");
      push(
        "decoded-traversal-escapes-home-blocked",
        !threw && lineage?.found === false && leakFree(lineage, PARENT_SENTINEL),
        `found=${lineage?.found} threw=${threw || "no"}`,
      );
    }

    // 3. Canonical minted ids (cand_ + 12-hex mem/idFor, + 16-hex
    //    knowledge/shortDigest) no longer reactivate direct private reads.
    {
      const { lineage, threw } = await trace("cand_9df96833602e");
      push(
        "canonical-12hex-private-read-retired",
        !threw && lineage?.found === false && !JSON.stringify(lineage).includes("entity:canonical-12"),
        `found=${lineage?.found} steps=${lineage?.steps.length} threw=${threw || "no"}`,
      );
    }
    {
      const { lineage, threw } = await trace("cand_0123456789abcdef");
      push(
        "canonical-16hex-private-read-retired",
        !threw && lineage?.found === false && !JSON.stringify(lineage).includes("entity:canonical-16"),
        `found=${lineage?.found} steps=${lineage?.steps.length} threw=${threw || "no"}`,
      );
    }

    // 4. Invalid id with a real file on disk: cand_x.json EXISTS, so
    //    found:false is only reachable if no filesystem read happened.
    {
      const { lineage, threw } = await trace("cand_x");
      push(
        "invalid-short-id-performs-no-read",
        !threw && lineage?.found === false && leakFree(lineage, DECOY_ENTITY),
        `found=${lineage?.found} threw=${threw || "no"}`,
      );
    }

    // 5. The minted grammar accepts exactly 12 or 16 hex characters, not the
    //    in-between lengths a `{12,16}` range would accidentally admit. Real
    //    decoy files make each check fail if an invalid length reaches readFile.
    for (const bad of ["cand_0123456789abc", "cand_0123456789abcd", "cand_0123456789abcde"]) {
      const { lineage, threw } = await trace(bad);
      push(
        `invalid-hex-length-performs-no-read:${bad.length - "cand_".length}`,
        !threw && lineage?.found === false && leakFree(lineage, INVALID_LENGTH_ENTITY),
        `found=${lineage?.found} threw=${threw || "no"}`,
      );
    }

    // 6. Encoded/alternate/short grammar forms stay fail-soft: found:false,
    //    never a throw, never a fabricated step.
    for (const bad of ["cand_..%2f..%2foutside", "cand_..\\..\\outside", "cand_9DF96833602E", "cand_"]) {
      const { lineage, threw } = await trace(bad);
      push(
        `invalid-form-fail-soft:${bad}`,
        !threw && lineage?.found === false && lineage.steps.length === 0,
        `found=${lineage?.found} steps=${lineage?.steps.length} threw=${threw || "no"}`,
      );
    }

    return {
      ok: checks.every((c) => c.ok),
      details: checks.map((c) => `${c.ok ? "ok  " : "FAIL"} ${c.name} — ${c.detail}`),
    };
  } finally {
    rmSync(tmpParent, { recursive: true, force: true });
  }
}

if ((import.meta as { main?: boolean }).main) {
  const result = await runLineageSelfTest();
  for (const line of result.details) console.log(line);
  if (!result.ok) {
    console.error("lineage selftest FAILED");
    process.exit(1);
  }
  console.log(
    "lineage selftest ok: private candidate reads stay retired for canonical and adversarial ids",
  );
}
