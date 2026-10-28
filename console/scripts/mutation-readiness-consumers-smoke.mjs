import assert from "node:assert/strict";
import fs from "node:fs";

const standardActionPages = [
  "src/app/page.tsx",
  "src/app/reviews/page.tsx",
  "src/app/memory/page.tsx",
  "src/app/routing/page.tsx",
  "src/app/sources/page.tsx",
];

for (const file of standardActionPages) {
  const source = fs.readFileSync(file, "utf8");
  assert.ok(
    source.includes("const mutationsEnabled = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;"),
    `${file} must expose only the exact effective .ready conjunction`,
  );
  assert.ok(
    source.includes("mutationsEnabled={mutationsEnabled}") || source.includes("mutationsReady={mutationsEnabled}"),
    `${file} must pass effective readiness into its live-action child`,
  );
}

const briefing = fs.readFileSync("src/app/page.tsx", "utf8");
assert.ok(
  briefing.includes("<ContinueCard model={continueModel} mutationsReady={mutationsEnabled} />"),
  "briefing must pass effective readiness into Continue",
);

const work = fs.readFileSync("src/app/work/page.tsx", "utf8");
assert.ok(
  work.includes("const mutationsReady = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;"),
  "work board must consume the exact effective .ready conjunction",
);
assert.ok(work.includes("mutationsReady={mutationsReady}"), "work board must pass effective readiness into drawers");
const detail = fs.readFileSync("src/app/work/[id]/page.tsx", "utf8");
assert.ok(
  detail.includes("const mutationsReady = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;"),
  "work detail must consume the exact effective .ready conjunction",
);
assert.ok(detail.includes("mutationsReady={mutationsReady}"), "work detail must pass effective readiness into actions");

const settings = fs.readFileSync("src/app/settings/page.tsx", "utf8");
assert.ok(
  settings.includes("const mutationReadiness = computeConsoleMutationReadiness(consoleMutationsEnabled());") &&
    settings.includes("mutationsEnabled={mutationReadiness.ready}") &&
    settings.includes("mutationFlagEnabled={mutationReadiness.flagEnabled}") &&
    settings.includes("instancePinned={mutationReadiness.instancePinned}") &&
    settings.includes("standardClientAuthorized={mutationReadiness.standardClientAuthorized}"),
  "settings must distinguish API opt-in, instance pin, and the projection-only standard client",
);

const consoleEnv = fs.readFileSync("src/components/settings/ConsoleEnv.tsx", "utf8");
const settingsData = fs.readFileSync("src/components/settings/data.ts", "utf8");
assert.doesNotMatch(
  `${consoleEnv}\n${settingsData}`,
  /one-click/i,
  "projection-only settings must describe server-adapter coverage, not advertise one-click client acts",
);
assert.ok(
  consoleEnv.includes('title="Server-adapter coverage"') && settingsData.includes("serverAdapterAreas"),
  "settings must name the server-adapter coverage seam explicitly",
);

console.log("mutation-readiness consumer smoke ok: every standard action page remains projection-only");
