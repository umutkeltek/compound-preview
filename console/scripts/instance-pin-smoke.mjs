import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { resolve } from "node:path";
// Keep the production readiness response covered by this existing proof gate.
import "./console-readiness.test.ts";

/**
 * Falsifier for the 2026-07-11 cross-port isolation breach (finding
 * rvf_7f1321314811; misattributed decisions cld_5eabdf8f72d6 / cld_9b63382def8d):
 * a console clone started against a scratch home on another port routed a
 * MUTATING act to the canonical ~/.compound. Root cause: the console resolved
 * COMPOUND_HOME fail-OPEN — an unset env silently bound the canonical home, and
 * nothing refused the mutation. This proves the pin: an instance is bound to ONE
 * explicit home at start, an unpinned instance CANNOT mutate (fails closed), and
 * a scratch-A pin never resolves to scratch-B or the canonical default.
 *
 * Runs under `node --experimental-strip-types`, so instance-pin.ts must import
 * only node: builtins (no local extensionless imports it cannot resolve).
 */

const { computeInstancePin, computeConsoleMutationReadiness, instanceUnpinnedBlockedResponse, INSTANCE_PIN } =
  await import("../src/lib/instance-pin.ts");
const { COMPOUND_HOME } = await import("../src/lib/compound.ts");

const canonical = resolve(`${homedir()}/.compound`);
const scratchA = "/scratch/A/.compound";
const scratchB = "/scratch/B/.compound";

// 1. Unset COMPOUND_HOME is the breach condition: it resolves to canonical, and
//    the pin flags it as NOT explicit so the mutation gate can fail closed.
const unpinned = computeInstancePin({});
assert.equal(unpinned.explicit, false, "unset COMPOUND_HOME must be flagged not-explicit");
assert.equal(unpinned.home, canonical, "unset COMPOUND_HOME resolves canonical (the fail-open default)");

// 2. An unpinned instance must REFUSE to mutate (fail closed) — this is the
//    guarantee that closes the breach: no explicit home, no write.
const blockedUnpinned = instanceUnpinnedBlockedResponse(unpinned);
assert.ok(blockedUnpinned, "unpinned instance must block mutations");
assert.equal(blockedUnpinned.status, 409, "unpinned mutation must fail closed with 409");

// 3. An explicitly pinned instance is allowed, and is pinned to its OWN home —
//    never scratch-B, never the canonical default.
const pinnedA = computeInstancePin({ COMPOUND_HOME: scratchA });
assert.equal(pinnedA.explicit, true, "explicit COMPOUND_HOME must be flagged explicit");
assert.equal(pinnedA.home, resolve(scratchA), "pin binds its own home");
assert.notEqual(pinnedA.home, resolve(scratchB), "scratch-A pin must never equal scratch-B");
assert.notEqual(pinnedA.home, canonical, "scratch-A pin must never equal the canonical default");
assert.equal(instanceUnpinnedBlockedResponse(pinnedA), null, "explicitly pinned instance may mutate");
assert.equal(
  computeInstancePin({ COMPOUND_HOME: scratchA, COMPOUND_CONSOLE_BUILD_KIND: "production" }).buildKind,
  "production",
  "instance identity must expose the production lifecycle kind",
);
assert.equal(
  computeInstancePin({ COMPOUND_HOME: scratchA, COMPOUND_CONSOLE_BUILD_KIND: "development" }).buildKind,
  "development",
  "instance identity must expose the development lifecycle kind",
);
assert.equal(pinnedA.buildKind, "unknown", "an unclassified runtime must not impersonate dev or production");

// 3b. Render-time readiness reports the extension flag and frozen instance pin
// but keeps the standard browser unauthorized. Neither input may advertise a
// live browser act because the capability is deliberately absent client-side.
assert.deepEqual(
  computeConsoleMutationReadiness(true, unpinned),
  { flagEnabled: true, instancePinned: false, standardClientAuthorized: false, ready: false },
  "enabled flag on an unpinned instance must remain read-only",
);
assert.deepEqual(
  computeConsoleMutationReadiness(false, pinnedA),
  { flagEnabled: false, instancePinned: true, standardClientAuthorized: false, ready: false },
  "pinned instance without the mutation flag must remain read-only",
);
assert.deepEqual(
  computeConsoleMutationReadiness(true, pinnedA),
  { flagEnabled: true, instancePinned: true, standardClientAuthorized: false, ready: false },
  "the standard browser client remains projection-only even when the API extension is configured",
);

// 4. Home identity: distinct homes hash distinct; two spellings of the same
//    absolute home hash identical (relative spellings can't smuggle a mismatch).
const pinnedB = computeInstancePin({ COMPOUND_HOME: scratchB });
assert.notEqual(pinnedA.homeHash, pinnedB.homeHash, "distinct homes must hash distinct");
assert.ok(/^[0-9a-f]+$/.test(pinnedA.homeHash), "home hash is short hex");
assert.equal(pinnedA.homeBasename, ".compound", "basename surfaces the home leaf");
const pinnedARel = computeInstancePin({ COMPOUND_HOME: "/scratch/A/../A/.compound" });
assert.equal(pinnedARel.homeHash, pinnedA.homeHash, "same absolute home => same hash regardless of spelling");

// 5. The exec home runCompound injects IS the frozen pin — one bound home, not a
//    per-request value. (Both read process.env at load; they must never drift.)
assert.equal(INSTANCE_PIN.home, COMPOUND_HOME, "runCompound exec home must equal the frozen instance pin");

// 6. The instance badge lives in the persistent shell, so that shell must keep
// its identity and controls reachable at narrow widths. These source-level
// contracts are deterministic proof for the responsive classes; the browser
// journey separately measures scrollWidth === innerWidth at 390px.
const layoutSource = readFileSync(new URL("../src/app/layout.tsx", import.meta.url), "utf8");
const refreshSource = readFileSync(new URL("../src/components/RefreshControl.tsx", import.meta.url), "utf8");
const themeSource = readFileSync(new URL("../src/components/kit/ThemeToggle.tsx", import.meta.url), "utf8");
const globalStyles = readFileSync(new URL("../src/app/globals.css", import.meta.url), "utf8");
const shellFailures = [];

function requireSource(source, pattern, message) {
  if (!pattern.test(source)) shellFailures.push(message);
}

requireSource(
  layoutSource,
  /<header className="[^"]*\bflex-wrap\b[^"]*\bsm:flex-nowrap\b/,
  "header needs a mobile wrap fallback and a single desktop row",
);
requireSource(
  layoutSource,
  /className="ml-auto flex shrink-0 items-center gap-1 sm:gap-2"/,
  "header controls need a compact non-shrinking group",
);
requireSource(
  layoutSource,
  /<ThemeToggle responsive\s*\/>/,
  "the shell must request ThemeToggle's compact mobile control",
);
requireSource(
  refreshSource,
  /data-refresh-control="expanded"/,
  "RefreshControl needs an explicit expanded surface",
);
requireSource(
  refreshSource,
  /data-refresh-control="compact"/,
  "RefreshControl needs an explicit compact surface",
);
requireSource(
  refreshSource,
  /\bshrink-0\b[^"\n]*\bwhitespace-nowrap\b|\bwhitespace-nowrap\b[^"\n]*\bshrink-0\b/,
  "desktop refresh pills must neither shrink nor wrap",
);
requireSource(
  themeSource,
  /data-theme-control="compact"/,
  "ThemeToggle needs a reachable compact mobile surface",
);
requireSource(
  themeSource,
  /data-theme-control="expanded"/,
  "ThemeToggle must preserve the expanded radio group from sm upward",
);

function parseDarkToken(name) {
  const dark = globalStyles.match(/:root\[data-theme="dark"\]\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";
  const value = dark.match(new RegExp(`--${name}:\\s*(#[0-9a-f]{6})`, "i"))?.[1];
  if (!value) shellFailures.push(`dark theme token --${name} is missing or not a six-digit hex`);
  return value ?? "#000000";
}

function relativeLuminance(hex) {
  const channels = [1, 3, 5].map((offset) => Number.parseInt(hex.slice(offset, offset + 2), 16) / 255);
  const [r, g, b] = channels.map((channel) =>
    channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4,
  );
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrastRatio(a, b) {
  const lighter = Math.max(relativeLuminance(a), relativeLuminance(b));
  const darker = Math.min(relativeLuminance(a), relativeLuminance(b));
  return (lighter + 0.05) / (darker + 0.05);
}

const darkFaint = parseDarkToken("faint");
const darkMuted = parseDarkToken("muted");
for (const surface of ["page", "panel", "soft", "soft-2"]) {
  const background = parseDarkToken(surface);
  const ratio = contrastRatio(darkFaint, background);
  if (ratio < 4.5) shellFailures.push(`dark --faint contrast on --${surface} is ${ratio.toFixed(2)}:1; need >= 4.5:1`);
}
if (relativeLuminance(darkFaint) >= relativeLuminance(darkMuted)) {
  shellFailures.push("dark --faint must remain visually subordinate to --muted");
}

assert.deepEqual(shellFailures, [], `console shell contract failures:\n${shellFailures.join("\n")}`);

console.log(
  "instance-pin smoke ok: mutations stay pinned; shell controls stay responsive; dark faint text keeps AA contrast",
);
