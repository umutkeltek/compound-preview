import { createHash } from "node:crypto";
import { homedir } from "node:os";
import { basename, resolve } from "node:path";

/**
 * Instance pin — the ONE COMPOUND_HOME a console server process is bound to,
 * captured once at module load (process start). Every mutating console act runs
 * against this home; a per-request value can never retarget it. This is the
 * guardrail for the 2026-07-11 cross-port isolation breach: a clone started on a
 * scratch home (another port) silently routed a mutation to the canonical
 * ~/.compound, because home resolution failed OPEN (unset env -> canonical) and
 * nothing refused the write.
 *
 * The fix has two halves that live here:
 *   1. `explicit` records whether COMPOUND_HOME was actually chosen at start. An
 *      unpinned instance (env unset) fails CLOSED for mutations — it would
 *      otherwise write the canonical home the operator never selected.
 *   2. `home` is absolute (resolved once), so two spellings of the same path
 *      cannot be confused and the badge/exec always agree on one identity.
 *
 * Node builtins only: this module is imported by the `node --experimental-strip-
 * types` console smokes, which cannot resolve local extensionless TS imports.
 */

const HOME_ENV = "COMPOUND_HOME";

export type InstancePin = {
  /** Absolute path of the bound home. */
  home: string;
  /** Leaf of the home path (e.g. ".compound"), for the operator-facing badge. */
  homeBasename: string;
  /** Short hex sha256 of the absolute home path — a stable instance fingerprint. */
  homeHash: string;
  /** True iff COMPOUND_HOME was explicitly set at start (not defaulted). */
  explicit: boolean;
  /** Lifecycle artifacts this process serves; unknown never impersonates a canonical runtime. */
  buildKind: "development" | "production" | "unknown";
};

export type ConsoleMutationReadiness = {
  flagEnabled: boolean;
  instancePinned: boolean;
  /** The standard browser never receives the server capability. */
  standardClientAuthorized: false;
  ready: boolean;
};

/**
 * Pure home->pin computation. Exported so the falsifier can drive both branches
 * (pinned / unpinned) without reloading the module.
 */
export function computeInstancePin(env: NodeJS.ProcessEnv = process.env): InstancePin {
  const raw = (env[HOME_ENV] ?? "").trim();
  const explicit = raw.length > 0;
  const home = resolve(explicit ? raw : `${homedir()}/.compound`);
  const homeHash = createHash("sha256").update(home).digest("hex").slice(0, 7);
  const rawBuildKind = (env.COMPOUND_CONSOLE_BUILD_KIND ?? "").trim();
  const buildKind = rawBuildKind === "development" || rawBuildKind === "production" ? rawBuildKind : "unknown";
  return { home, homeBasename: basename(home) || home, homeHash, explicit, buildKind };
}

/** Frozen at module load: the pin for THIS server process. */
export const INSTANCE_PIN: InstancePin = computeInstancePin(process.env);

/**
 * The one render-time mutation readiness contract for the standard browser.
 * The public-alpha UI never receives the server's out-of-band capability, so
 * it remains projection-only even when an operator configures the unsupported
 * local API extension. Direct extension callers authenticate separately at
 * the request gate in console-mutations.ts.
 */
export function computeConsoleMutationReadiness(
  flagEnabled: boolean,
  pin: InstancePin = INSTANCE_PIN,
): ConsoleMutationReadiness {
  const instancePinned = pin.explicit;
  return { flagEnabled, instancePinned, standardClientAuthorized: false, ready: false };
}

/**
 * Mutation gate: refuse to mutate when the serving instance was not started with
 * an explicit COMPOUND_HOME. Without this, an unpinned clone silently writes the
 * canonical ~/.compound — the exact 2026-07-11 breach. Fails closed with a 409
 * so a click can never land on a home the operator did not choose.
 */
export function instanceUnpinnedBlockedResponse(pin: InstancePin = INSTANCE_PIN): Response | null {
  if (pin.explicit) return null;
  return Response.json(
    {
      ok: false,
      stderr:
        "console mutation refused: this server was started without an explicit COMPOUND_HOME, so " +
        "a click would silently write the canonical ~/.compound instead of the instance you launched. " +
        "Start it via `compound console --operator` (or set COMPOUND_HOME) so the instance is pinned.",
    },
    { status: 409 },
  );
}
